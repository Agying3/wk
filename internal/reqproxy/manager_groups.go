// Manager 的代理分组管理：分组 CRUD、订阅↔专属分组关联、区域合规清扫。
//
// 语义（见设计文档"代理分组"章节）：
//   - 订阅创建时必须选区域（cn|global）→ 自动建 1:1 专属分组并关联；
//     删除订阅连带删组。专属组不可改名（名随订阅）、不可经通用接口删除。
//   - 手动组可建/改名/改区域/删除；删除时其手动节点归未分组。
//   - 区域变更/挪节点可能让既有槽位违规 → SweepRegionViolations 修正
//     （交集 Retarget，端口不变；无解时告警保留）。
package reqproxy

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// validProxyGroupRegion 代理分组区域允许 cn|global|all：
//   - cn/global：分组节点只服务对应区域的账号槽位（结构性隔离）；
//   - all：不限制区域，对两类账号槽位都可见（默认分组即 all）。
func validProxyGroupRegion(r string) bool {
	return r == "cn" || r == "global" || r == RegionAll
}

// CreateProxyGroup 新建手动代理分组。
func (m *Manager) CreateProxyGroup(name, region string) (ProxyGroup, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProxyGroup{}, fmt.Errorf("分组名不能为空")
	}
	if len([]rune(name)) > 32 {
		return ProxyGroup{}, fmt.Errorf("分组名过长（最多 32 字符）")
	}
	if !validProxyGroupRegion(region) {
		return ProxyGroup{}, fmt.Errorf("代理分组区域必须是 cn、global 或 all")
	}
	g := ProxyGroup{ID: newID("pg"), Name: name, Region: region, CreatedAt: time.Now()}
	m.store.Update(func(st *State) {
		st.ProxyGroups = append(st.ProxyGroups, g)
	})
	m.emit("info", fmt.Sprintf("代理分组 %q（%s 区）已创建", name, region))
	return g, nil
}

// RenameProxyGroup 改名（订阅专属组不可改名——名随订阅；默认分组是
// 系统内置标识，不可改名）。
func (m *Manager) RenameProxyGroup(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("分组名不能为空")
	}
	var err error
	m.store.Update(func(st *State) {
		for i := range st.ProxyGroups {
			if st.ProxyGroups[i].ID == id {
				if st.ProxyGroups[i].SubID != "" {
					err = fmt.Errorf("订阅专属分组不可改名（名称跟随订阅）")
					return
				}
				if id == DefaultProxyGroupID {
					err = fmt.Errorf("默认分组不可改名")
					return
				}
				st.ProxyGroups[i].Name = name
				return
			}
		}
		err = fmt.Errorf("分组 %s 不存在", id)
	})
	return err
}

// SetProxyGroupRegion 改分组区域。
//
// 订阅专属组：区域即订阅区域，同步改订阅记录；手动组：直接改。
// 区域变更后调用 SweepRegionViolations 修正违规槽位（由 server 层触发，
// 这里不自动——server 要先做账号分组绑定方向的合规校验）。
func (m *Manager) SetProxyGroupRegion(id, region string) error {
	if id == DefaultProxyGroupID {
		return fmt.Errorf("默认分组区域固定为 all，不可修改")
	}
	if !validProxyGroupRegion(region) {
		return fmt.Errorf("代理分组区域必须是 cn、global 或 all")
	}
	var err error
	m.store.Update(func(st *State) {
		for i := range st.ProxyGroups {
			if st.ProxyGroups[i].ID == id {
				st.ProxyGroups[i].Region = region
				// 订阅专属组：区域同步回订阅（两者是同一事实的两面）。
				if sub := st.ProxyGroups[i].SubID; sub != "" {
					for j := range st.Subscriptions {
						if st.Subscriptions[j].ID == sub {
							st.Subscriptions[j].GroupID = id
						}
					}
				}
				return
			}
		}
		err = fmt.Errorf("分组 %s 不存在", id)
	})
	if err != nil {
		return err
	}
	m.rebuildPool() // 区域变化经 GroupRegionIndex 影响候选
	m.SweepRegionViolations()
	return nil
}

// DeleteProxyGroup 删除手动分组（订阅专属组必须删订阅；默认分组不可删）。
// 返回受影响的手动节点数。组内手动节点回落到默认分组（不再允许未分组）。
func (m *Manager) DeleteProxyGroup(id string) (int, error) {
	if id == DefaultProxyGroupID {
		return 0, fmt.Errorf("默认分组不可删除")
	}
	affected := 0
	var err error
	m.store.Update(func(st *State) {
		idx := -1
		for i := range st.ProxyGroups {
			if st.ProxyGroups[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			err = fmt.Errorf("分组 %s 不存在", id)
			return
		}
		if st.ProxyGroups[idx].SubID != "" {
			err = fmt.Errorf("订阅专属分组不可单独删除（删除对应订阅即可）")
			return
		}
		st.ProxyGroups = append(st.ProxyGroups[:idx], st.ProxyGroups[idx+1:]...)
		// 组内手动节点回落到默认分组（未分组不再是合法持久态）。
		for i := range st.ManualNodes {
			if st.ManualNodes[i].GroupID == id {
				st.ManualNodes[i].GroupID = DefaultProxyGroupID
				affected++
			}
		}
	})
	if err != nil {
		return 0, err
	}
	m.rebuildPool()
	m.SweepRegionViolations()
	m.emit("info", fmt.Sprintf("代理分组 %s 已删除（%d 个手动节点回落默认分组）", id, affected))
	return affected, nil
}

// SetManualNodesGroup 批量挪组：一次 Update 改完所有节点，一次 rebuild +
// Sweep（对比逐节点调用 = N 次全池重建）。groupID 空 = 挪回默认分组。
// 返回实际挪动的节点数；nodeIDs 里的订阅节点/不存在节点跳过（不报错，
// 前端勾选可能混入）。
func (m *Manager) SetManualNodesGroup(nodeIDs []string, groupID string) (int, error) {
	if groupID == "" {
		groupID = DefaultProxyGroupID
	}
	var err error
	moved := 0
	m.store.Update(func(st *State) {
		if st.ProxyGroupByID(groupID) == nil {
			err = fmt.Errorf("代理分组 %s 不存在", groupID)
			return
		}
		if g := st.ProxyGroupByID(groupID); g != nil && g.SubID != "" {
			err = fmt.Errorf("订阅专属分组的成员由订阅决定，不能手动加入")
			return
		}
		want := map[string]bool{}
		for _, id := range nodeIDs {
			want[id] = true
		}
		for i := range st.ManualNodes {
			if want[st.ManualNodes[i].ID] && st.ManualNodes[i].GroupID != groupID {
				st.ManualNodes[i].GroupID = groupID
				moved++
			}
		}
	})
	if err != nil {
		return 0, err
	}
	if moved > 0 {
		m.rebuildPool()
		m.SweepRegionViolations()
	}
	return moved, nil
}

// SetManualNodeGroup 手动节点挪组。groupID 空 = 挪回默认分组
// （未分组不再是合法状态；默认分组 all 区，行为等同旧版"不分组"）。
func (m *Manager) SetManualNodeGroup(nodeID, groupID string) error {
	var err error
	m.store.Update(func(st *State) {
		if groupID == "" {
			groupID = DefaultProxyGroupID
		}
		if st.ProxyGroupByID(groupID) == nil {
			err = fmt.Errorf("代理分组 %s 不存在", groupID)
			return
		}
		if g := st.ProxyGroupByID(groupID); g != nil && g.SubID != "" {
			err = fmt.Errorf("订阅专属分组的成员由订阅决定，不能手动加入")
			return
		}
		for i := range st.ManualNodes {
			if st.ManualNodes[i].ID == nodeID {
				st.ManualNodes[i].GroupID = groupID
				return
			}
		}
		err = fmt.Errorf("手动节点 %s 不存在", nodeID)
	})
	if err != nil {
		return err
	}
	m.rebuildPool()
	m.SweepRegionViolations()
	return nil
}

// ensureSubscriptionGroup 给订阅建专属分组（不存在时）。返回分组 ID。
// 同名分组名冲突不敏感——展示名可重复，ID 才是键。
func (m *Manager) ensureSubscriptionGroup(st *State, subID, subName, region string) string {
	for i := range st.ProxyGroups {
		if st.ProxyGroups[i].SubID == subID {
			// 已有专属组：区域对齐（订阅区域与组区域是同一事实）。
			st.ProxyGroups[i].Region = region
			return st.ProxyGroups[i].ID
		}
	}
	g := ProxyGroup{ID: newID("pg"), Name: subName, Region: region, SubID: subID, CreatedAt: time.Now()}
	st.ProxyGroups = append(st.ProxyGroups, g)
	return g.ID
}

// AddSubscription 新建订阅（带区域）→ 自动建专属分组。
func (m *Manager) AddSubscription(name, rawURL string, autoRefresh bool, interval, region string) (Subscription, error) {
	if !validProxyGroupRegion(region) {
		return Subscription{}, fmt.Errorf("订阅区域必须是 cn 或 global")
	}
	sub := NewSubscription(name, rawURL, autoRefresh, interval)
	m.store.Update(func(st *State) {
		sub.GroupID = m.ensureSubscriptionGroup(st, sub.ID, sub.Name, region)
		st.Subscriptions = append(st.Subscriptions, sub)
	})
	m.emit("info", fmt.Sprintf("订阅 %q 已添加（%s 区，专属分组 %s）", name, region, sub.GroupID))
	return sub, nil
}

// SetSubscriptionRegion 设置/变更订阅区域：无专属组则建组关联；有则改组区域。
func (m *Manager) SetSubscriptionRegion(subID, region string) error {
	if !validProxyGroupRegion(region) {
		return fmt.Errorf("订阅区域必须是 cn 或 global")
	}
	var err error
	m.store.Update(func(st *State) {
		idx := -1
		for i := range st.Subscriptions {
			if st.Subscriptions[i].ID == subID {
				idx = i
				break
			}
		}
		if idx < 0 {
			err = fmt.Errorf("订阅 %s 不存在", subID)
			return
		}
		gid := m.ensureSubscriptionGroup(st, subID, st.Subscriptions[idx].Name, region)
		st.Subscriptions[idx].GroupID = gid
	})
	if err != nil {
		return err
	}
	m.rebuildPool()
	m.SweepRegionViolations()
	return nil
}

// DeleteSubscription 删除订阅并连带删除其专属分组。返回被删分组的 ID
// （空 = 订阅本来就没有专属分组，旧数据）。
func (m *Manager) DeleteSubscription(id string) string {
	pgID := ""
	m.store.Update(func(st *State) {
		for i, s := range st.Subscriptions {
			if s.ID == id {
				pgID = s.GroupID
				st.Subscriptions = append(st.Subscriptions[:i], st.Subscriptions[i+1:]...)
				break
			}
		}
		if pgID != "" {
			for i, g := range st.ProxyGroups {
				if g.ID == pgID {
					st.ProxyGroups = append(st.ProxyGroups[:i], st.ProxyGroups[i+1:]...)
					break
				}
			}
		}
	})
	m.rebuildPool()
	m.SweepRegionViolations()
	if pgID != "" {
		m.emit("info", fmt.Sprintf("订阅已删除，专属分组 %s 一并移除", pgID))
	}
	return pgID
}

// ProxyGroupsSnapshot 全部代理分组（快照，API 用）。
func (m *Manager) ProxyGroupsSnapshot() []ProxyGroup {
	var out []ProxyGroup
	m.store.View(func(st *State) {
		out = append(out, st.ProxyGroups...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SweepRegionViolations 区域/分组变更后的合规清扫：槽位指向的节点不再
// 服务槽位区域（D3）时换指向。端口不变（账号零感知）；无替代节点保留
// 原状并发 warn 事件（管理员手动处理或等节点恢复）。
func (m *Manager) SweepRegionViolations() int {
	if m.kernel == nil && m.store == nil {
		return 0
	}
	slots, _ := m.pool.Views()
	fixed := 0
	for _, s := range slots {
		if s.NodeID == "" {
			continue
		}
		node := m.pool.nodeByID(s.NodeID)
		if node == nil {
			continue
		}
		if m.pool.nodeServesRegion(*node, s.Region) {
			continue
		}
		op, err := m.pool.Retarget(s.ID, s.Region)
		if err != nil || op == nil {
			m.emit("warn", fmt.Sprintf("槽位 %s 指向 %s 区违规节点 %s 且无合规替代，暂保留（账号 %s 区请求将重选）",
				s.ID, s.Region, node.Name, s.Region))
			continue
		}
		if m.kernel != nil {
			if err := m.kernel.RetargetSlot(op.SlotID, op.NodeSpec); err != nil {
				m.emit("warn", fmt.Sprintf("槽位 %s 违规换向失败（内核）: %v", s.ID, err))
				continue
			}
		}
		fixed++
		m.emit("slot-switch", fmt.Sprintf("槽位 %s 从区域违规节点 %s 换向 %s（端口不变）", s.ID, node.Name, op.NodeSpec.Name))
	}
	return fixed
}
