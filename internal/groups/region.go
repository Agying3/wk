// 代理分组 + 分组区域（CN/国际）的持久化扩展。
//
// 在既有三份 JSON（groups.json / account-groups.json / keys.json）之外新增
// 两份 sidecar 文件，放在同一数据目录：
//
//   - group-regions.json:   {"<group>": "cn|global|all", ...}
//   - proxy-bindings.json:  {"<group>": ["pg_xxx", ...], ...}  账号分组 → 代理分组 ID
//
// 兼容性：两份文件缺失时按空数据初始化——所有存量分组的区域都是 "all"
// （容纳两种账号、可绑定两种代理分组），代理绑定一律为空（不限制）。
// 这保证升级后行为与旧版完全一致，区域与绑定由管理员逐步设置。
//
// 引用方式：代理分组按 ID（pg_xxxx）引用而非名称——代理分组改名
// （仅手动组允许，订阅组名随订阅）不影响绑定。
package groups

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 区域取值。
const (
	RegionCN     = "cn"     // 中国区
	RegionGlobal = "global" // 国际区
	RegionAll    = "all"    // 未区分（迁移默认态）：容纳两种账号、可绑定两种代理分组
)

// ValidRegion 校验区域取值（all 仅对账号分组合法；代理分组创建时必选 cn/global）。
func ValidRegion(r string, allowAll bool) bool {
	switch r {
	case RegionCN, RegionGlobal:
		return true
	case RegionAll:
		return allowAll
	}
	return false
}

// RegionCompatible 区域兼容：a 能容纳/绑定 b。
// all 与任何区域兼容；否则必须同区。
func RegionCompatible(a, b string) bool {
	if a == RegionAll || b == RegionAll {
		return true
	}
	return a == b
}

// GroupInfo 列表视图项：分组名 + 区域。
type GroupInfo struct {
	Name        string    `json:"name"`
	Default     bool      `json:"default"`
	Region      string    `json:"region"`       // cn|global|all
	ProxyGroups []string  `json:"proxy_groups"` // 绑定的代理分组 ID（去重有序）
	CreatedAt   time.Time `json:"-"`            // groups.json 历史格式不存时间，占位
}

// CreateWithRegion 新建分组并指定区域。region ∈ cn|global|all。
func (s *Store) CreateWithRegion(name, region string) error {
	name = strings.TrimSpace(name)
	if err := s.ValidateName(name); err != nil {
		return err
	}
	if !ValidRegion(region, true) {
		return fmt.Errorf("分组区域不合法（cn/global/all）: %q", region)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = append(s.groups, name)
	s.regions[name] = region
	s.dirtyReg = true
	return s.persistLocked()
}

// Create 新建分组（区域 all——兼容旧调用方；迁移后应改用 CreateWithRegion）。
func (s *Store) Create(name string) error {
	return s.CreateWithRegion(name, RegionAll)
}

// RegionOf 查询分组区域。未知分组返回 all（读路径兜底，不报错）。
func (s *Store) RegionOf(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.regions[name]; ok && r != "" {
		return r
	}
	return RegionAll
}

// SetGroupRegion 设置分组区域。default 名字不可改但区域可改（迁移需要）。
//
// 成员校验由调用方（server 层）完成——这里只管存储；理由：成员账号区域
// 需要 Pool/Auth 依赖，groups 包不引入。
func (s *Store) SetGroupRegion(name, region string) error {
	if !ValidRegion(region, true) {
		return fmt.Errorf("分组区域不合法（cn/global/all）: %q", region)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGroupLocked(name) {
		return ErrNotFound
	}
	s.regions[name] = region
	s.dirtyReg = true
	return s.persistLocked()
}

// ListDetailed 全部分组 + 区域 + 代理绑定（管理页渲染）。
func (s *Store) ListDetailed() []GroupInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GroupInfo, 0, len(s.groups))
	for _, g := range s.groups {
		region := s.regions[g]
		if region == "" {
			region = RegionAll
		}
		binds := s.pgBindings[g]
		cp := append([]string(nil), binds...)
		sort.Strings(cp)
		out = append(out, GroupInfo{Name: g, Default: g == DefaultGroup, Region: region, ProxyGroups: cp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default {
			return true
		}
		if out[j].Default {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// —— 代理分组绑定（账号分组 → 代理分组 ID，多对多） ——

// SetProxyBindings 设置账号分组绑定的代理分组集合（全量替换）。
// 分组必须存在；pgID 不做存在性校验（reqproxy 侧的状态，groups 包不依赖），
// 失效引用由解析方忽略、删除代理分组时由 server 调 RemoveProxyGroupRef 清理。
func (s *Store) SetProxyBindings(group string, pgIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGroupLocked(group) {
		return ErrNotFound
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(pgIDs))
	for _, id := range pgIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	sort.Strings(clean)
	// 显式清空（传入非 nil 空列表 / 有内容的空结果）要落盘：保留空列表
	// 让"管理员清空过绑定"与"从未绑定"可区分（后者会被 default 种子重新
	// 写入）。nil 入参（调用方未提供）且键不存在时才视为无变化。
	if len(clean) == 0 && !s.hasBindingLocked(group) && pgIDs == nil {
		return nil
	}
	s.pgBindings[group] = clean
	s.dirtyBind = true
	return s.persistLocked()
}

// ProxyBindingsOf 查询账号分组绑定的代理分组 ID（快照）。
func (s *Store) ProxyBindingsOf(group string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.pgBindings[group]...)
	sort.Strings(out)
	return out
}

// HasProxyBindings 报告账号分组是否有绑定记录（即便列表已被清空）。
// 用于区分"从未绑定"（可种子默认绑定）与"管理员显式清空"（不再种子）。
func (s *Store) HasProxyBindings(group string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasBindingLocked(group)
}

// SnapshotProxyBindings 导出全量绑定（管理页/测试用）。
func (s *Store) SnapshotProxyBindings() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.pgBindings))
	for g, ids := range s.pgBindings {
		out[g] = append([]string(nil), ids...)
	}
	return out
}

// RemoveProxyGroupRef 从所有账号分组的绑定里移除对某代理分组的引用。
// 返回受影响的账号分组数（删除代理分组后清理用）。
func (s *Store) RemoveProxyGroupRef(pgID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected := 0
	for g, ids := range s.pgBindings {
		next := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != pgID {
				next = append(next, id)
			}
		}
		if len(next) != len(ids) {
			affected++
			if len(next) == 0 {
				delete(s.pgBindings, g)
			} else {
				s.pgBindings[g] = next
			}
		}
	}
	if affected > 0 {
		_ = s.persistLocked()
	}
	return affected
}

func (s *Store) hasBindingLocked(group string) bool {
	_, ok := s.pgBindings[group]
	return ok
}

// RegisteredGroups 返回账号在归属表里**显式登记**的分组。
// 与 AccountGroups 的区别：不做 default 回落——用于代理绑定解析
// （未登记账号不受绑定限制，与 keys.json 选号只看归属表的语义一致）。
func (s *Store) RegisteredGroups(uid string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if gs, ok := s.accountOf[uid]; ok && len(gs) > 0 {
		return append([]string(nil), gs...)
	}
	return nil
}
