// Package haozhumah5 访问豪猪网页版后端（2026-10 改版：h5.haozhuma.cn + api.veeto.cn/v1）。
//
// 背景：2026-10 上旬豪猪因 php2shell 漏洞被攻击，旧 PHP 站（h5.haozhuma.com
// 的 api.php?type=30/8/4/3 + PHPSESSID）整体废弃，账号数据也未能完整恢复，
// 官方要求重新注册。新站是 React SPA，后端换成 api.veeto.cn/v1，鉴权从
// PHPSESSID Cookie 改为 Authorization: Bearer <token>（网页登录颁发，7 天）。
//
// 与 internal/haozhuma（官方接码 API，账密换 token）平行：这里走网页端，
// 只做增强能力（项目搜索 / 对接码选择器数据源 + 加码/删码），核心加号链路
// 永远只依赖官方接码 API。token 未配置或失效时上层自动退化为手填模式。
//
// 端点（实测，见 docs/haozhuma-veeto-api.md）：
//
//	POST /v1/captcha                       取点选验证码（登录前，本包不实现）
//	POST /v1/auth/login                    网页登录换 token（本包不实现）
//	GET  /v1/projects/search?keyword=&size 项目搜索 → {list:[{id,sid,name}]}
//	GET  /v1/projects/{sid}/public-codes   项目下对接码挂牌列表（价格/库存/运营商）
//	GET  /v1/bindings?page=&size=          我已加入的对接码
//	POST /v1/bindings {"codes":[...]}      批量加入
//	POST /v1/bindings/batch {"action":...,"codes":[...]}  批量删除/暂停/恢复
//
// 注意：public-codes 必须用服务端签发的 16 位 hex sid，裸数字项目 ID 一律
// 返回空列表（实测）。项目数字 ID 用于官方 getPhone 的 sid 参数。
//
// token 生命周期：7 天硬过期，刷新必须重过点选验证码，无法自动化续期。
// 因此不做保活，过期后由上层提示重新粘贴（比旧 PHPSESSID 少一层保活逻辑）。
package haozhumah5

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BaseURL veeto API 基址。变量而非常量：测试里指到 httptest 服务器。
var BaseURL = "https://api.veeto.cn/v1"

// cacheTTL 搜索结果缓存时长。库存变化快但没必要每次开选择器都打上游。
const cacheTTL = 5 * time.Minute

// ErrNoSession 网页 token 未配置。
var ErrNoSession = errors.New("豪猪网页 token 未配置（需在 WebUI 粘贴网页登录 token）")

// ErrSessionExpired token 已失效（401/403 或 code=401）。
// 上层据此提示重新粘贴并退化为手填；已存的 token 不清空（可能只是临时故障）。
var ErrSessionExpired = errors.New("豪猪网页 token 已失效（7 天过期，请重新登录网页复制 token）")

// Project 项目搜索结果。
type Project struct {
	// SID 服务端签发的 16 位 hex 项目引用，用于查对接码。
	// 裸数字 ID 查码返回空，必须用这个。
	SID string `json:"sid"`
	// ProjectID 项目数字 ID（官方 getPhone 的 sid 参数）。新站直接给结构化
	// 字段，不再需要从「【52283】腾讯科技」名称里解析。
	ProjectID string `json:"project_id"`
	// Name 项目名称。
	Name string `json:"name"`
}

// UIDItem 对接码列表项。字段与旧版保持一致，前端无需改动。
type UIDItem struct {
	UID         string   `json:"uid"`          // 对接码（61903-MDGCVYBLNZ）
	Name        string   `json:"name"`         // 项目名称
	Price       float64  `json:"price"`        // 单价（元）
	Stock       int      `json:"stock"`        // 可用数量（-1=未知）
	ISPs        []string `json:"isps"`         // 运营商（中文）
	Provinces   []string `json:"provinces"`    // 省份限制（空=不限）
	SegmentType string   `json:"segment_type"` // 号段类型
	Segments    []string `json:"segments"`     // 号段前缀
	Pinned      bool     `json:"pinned"`       // 置顶
	UpdatedAt   string   `json:"updated_at"`   // 上游更新时间（原样）
}

// Client 网页端客户端。零值不可用，用 New 建。
//
// 并发安全：所有方法可被多个 goroutine 同时调。
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration

	mu        sync.Mutex
	session   string // Bearer token；空 = 未配置
	expired   bool
	lastSeen  time.Time
	lastError string

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	at    time.Time
	value any
}

// New 建客户端。session 为空串时客户端仍可用，但业务方法返回 ErrNoSession。
func New(session string) *Client {
	return &Client{
		HTTP:    newHTTPClient(),
		Timeout: 15 * time.Second,
		session: strings.TrimSpace(session),
		cache:   make(map[string]cacheEntry),
	}
}

// Close 释放资源。新站无保活 goroutine，这里只做接口兼容。
func (c *Client) Close() {
	// 无后台任务；保留方法以便上层代码不用改。
}

// Token 返回当前 token（脱敏由调用方负责）。
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Session 更新 token 并清失效标记与缓存。
func (c *Client) Session(s string) {
	c.mu.Lock()
	c.session = strings.TrimSpace(s)
	c.expired = false
	c.lastError = ""
	c.mu.Unlock()
	c.flushCache()
}

// StartKeepalive 兼容旧接口。新站 token 7 天硬过期且刷新需过验证码，
// 无法自动续期，此方法为空实现。
func (c *Client) StartKeepalive() {}

// LastError 上次错误文案（诊断回显）。
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

// Info 客户端状态（WebUI 回显用，字段与旧版保持一致）。
type Info struct {
	// Has 是否已配置 token。
	Has bool `json:"has"`
	// Expired 最近一次调用判定 token 失效。
	Expired bool `json:"expired"`
	// LastError 上次错误文案。
	LastError string `json:"last_error,omitempty"`
	// LastSeen 上次成功调用时间（零值=从未成功）。
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Info 返回客户端状态快照。
func (c *Client) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Info{
		Has:       c.session != "",
		Expired:   c.expired,
		LastError: c.lastError,
		LastSeen:  c.lastSeen,
	}
}

// Ping 探活：拉一次项目搜索验证 token 是否仍有效。
// 新站没有独立的轻量探活接口，用最小搜索代替（有 5 分钟缓存兜底）。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Projects(ctx, "腾讯")
	return err
}

// InvalidateUIDLists 清空对接码相关缓存（监控要的是"此刻"的库存，
// 通用 5 分钟 TTL 会制造假事件/漏事件）。
func (c *Client) InvalidateUIDLists() {
	c.cacheMu.Lock()
	for k := range c.cache {
		if strings.HasPrefix(k, "codes:") || strings.HasPrefix(k, "bindings:") {
			delete(c.cache, k)
		}
	}
	c.cacheMu.Unlock()
}

// LastSeen 上次成功调用时间。
func (c *Client) LastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen
}

// Expired token 是否已被判定失效。
func (c *Client) Expired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expired
}

// ---- 业务方法 ----

// Projects 项目搜索（GET /v1/projects/search）。
// 关键词为空时上游可能返回全量，这里直接拒绝，避免误拉。
func (c *Client) Projects(ctx context.Context, keyword string) ([]Project, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, errors.New("搜索关键词不能为空")
	}
	key := "search:" + keyword
	if v, ok := c.cached(key); ok {
		return *(v.(*[]Project)), nil
	}
	q := url.Values{}
	q.Set("keyword", keyword)
	q.Set("size", "50")
	var raw struct {
		List []struct {
			ID   string `json:"id"`
			SID  string `json:"sid"`
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := c.get(ctx, "/projects/search", q, &raw); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(raw.List))
	for _, it := range raw.List {
		name := strings.TrimSpace(it.Name)
		out = append(out, Project{
			SID:       strings.TrimSpace(it.SID),
			ProjectID: strings.TrimSpace(it.ID),
			Name:      name,
		})
	}
	// 保持上游原有顺序：搜索结果是按相关度排的，重新排序会把最相关的
	// 项目挤走（用户搜「腾讯科技」应当先看到「腾讯科技[限对接]」）。
	c.store(key, &out)
	return out, nil
}

// UIDs 项目下的对接码挂牌列表（GET /v1/projects/{sid}/public-codes）。
// sid 必须是 Projects 返回的 16 位 hex；传数字 ID 上游返回空列表。
func (c *Client) UIDs(ctx context.Context, sid string) ([]UIDItem, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return nil, errors.New("项目标识 sid 不能为空")
	}
	key := "codes:" + sid
	if v, ok := c.cached(key); ok {
		return *(v.(*[]UIDItem)), nil
	}
	var raw []struct {
		Code        string   `json:"code"`
		ProjectID   string   `json:"projectId"`
		ProjectName string   `json:"projectName"`
		Price       string   `json:"price"`
		Available   int      `json:"available"`
		Operators   []string `json:"operators"`
		Provinces   []string `json:"provinces"`
		Prefixes    []string `json:"prefixes"`
		SegmentType string   `json:"segmentType"`
		Pinned      bool     `json:"pinned"`
	}
	if err := c.get(ctx, "/projects/"+url.PathEscape(sid)+"/public-codes", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]UIDItem, 0, len(raw))
	for _, it := range raw {
		price, _ := strconv.ParseFloat(strings.TrimSpace(it.Price), 64)
		out = append(out, UIDItem{
			UID:         strings.TrimSpace(it.Code),
			Name:        strings.TrimSpace(it.ProjectName),
			Price:       price,
			Stock:       it.Available,
			ISPs:        it.Operators,
			Provinces:   it.Provinces,
			SegmentType: strings.TrimSpace(it.SegmentType),
			Segments:    it.Prefixes,
			Pinned:      it.Pinned,
		})
	}
	// 有货优先、同库存按价格低→高（取号先挑便宜的）。
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Stock > 0) != (b.Stock > 0) {
			return a.Stock > 0
		}
		if a.Stock != b.Stock {
			return a.Stock > b.Stock
		}
		return a.Price < b.Price
	})
	c.store(key, &out)
	return out, nil
}

// MyUIDs 我账户已加入的对接码（GET /v1/bindings 全量翻页）。
// q 非空时按项目名/码值本地过滤。
func (c *Client) MyUIDs(ctx context.Context, q string) ([]UIDItem, error) {
	key := "bindings:all"
	var items []UIDItem
	if v, ok := c.cached(key); ok {
		items = *(v.(*[]UIDItem))
	} else {
		const size = 100
		for page := 1; page <= 20; page++ { // 上限 2000 条，防失控
			qv := url.Values{}
			qv.Set("page", strconv.Itoa(page))
			qv.Set("size", strconv.Itoa(size))
			var raw struct {
				List []struct {
					Code           string   `json:"code"`
					ProjectName    string   `json:"projectName"`
					Price          string   `json:"price"`
					Available      int      `json:"available"`
					Operators      []string `json:"operators"`
					Provinces      []string `json:"provinces"`
					Prefixes       []string `json:"prefixes"`
					SegmentType    string   `json:"segmentType"`
					Pinned         bool     `json:"pinned"`
				} `json:"list"`
				Total int `json:"total"`
			}
			if err := c.get(ctx, "/bindings", qv, &raw); err != nil {
				return nil, err
			}
			for _, it := range raw.List {
				price, _ := strconv.ParseFloat(strings.TrimSpace(it.Price), 64)
				items = append(items, UIDItem{
					UID:         strings.TrimSpace(it.Code),
					Name:        strings.TrimSpace(it.ProjectName),
					Price:       price,
					Stock:       it.Available,
					ISPs:        it.Operators,
					Provinces:   it.Provinces,
					SegmentType: strings.TrimSpace(it.SegmentType),
					Segments:    it.Prefixes,
					Pinned:      it.Pinned,
				})
			}
			if len(raw.List) < size || len(items) >= raw.Total {
				break
			}
		}
		c.store(key, &items)
	}
	if q = strings.TrimSpace(q); q != "" {
		needle := strings.ToLower(q)
		filtered := make([]UIDItem, 0, len(items))
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Name), needle) ||
				strings.Contains(strings.ToLower(it.UID), needle) {
				filtered = append(filtered, it)
			}
		}
		return filtered, nil
	}
	return items, nil
}

// AddUID 把对接码加入账户（POST /v1/bindings）。
// 官方 getPhone?uid= 只认已加入的码，市场挂牌的码必须先加入。
func (c *Client) AddUID(ctx context.Context, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("对接码不能为空")
	}
	var raw struct {
		Results []struct {
			Code    string `json:"code"`
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"results"`
	}
	if err := c.post(ctx, "/bindings", map[string]any{"codes": []string{uid}}, &raw); err != nil {
		return err
	}
	for _, r := range raw.Results {
		if strings.TrimSpace(r.Code) != uid {
			continue
		}
		if !r.OK {
			msg := strings.TrimSpace(r.Message)
			if msg == "" {
				msg = "加入失败"
			}
			return fmt.Errorf("加入对接码 %s 失败：%s", uid, msg)
		}
		c.flushCache() // 我的对接码缓存作废
		return nil
	}
	// 上游没回这条码的结果：按失败处理，别让上层以为成功。
	return fmt.Errorf("加入对接码 %s 失败：上游未返回该码的结果", uid)
}

// RemoveUIDs 批量删除对接码（POST /v1/bindings/batch action=delete）。
// 返回受影响条数被调用方忽略（旧接口签名单返回值），故只回 error。
func (c *Client) RemoveUIDs(ctx context.Context, uids []string) error {
	_, err := c.batch(ctx, "delete", uids)
	return err
}

// PauseUIDs 批量暂停（action=pause）。
func (c *Client) PauseUIDs(ctx context.Context, uids []string) error {
	_, err := c.batch(ctx, "pause", uids)
	return err
}

// ResumeUIDs 批量恢复（action=resume）。
func (c *Client) ResumeUIDs(ctx context.Context, uids []string) error {
	_, err := c.batch(ctx, "resume", uids)
	return err
}

func (c *Client) batch(ctx context.Context, action string, uids []string) (int, error) {
	clean := make([]string, 0, len(uids))
	for _, u := range uids {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return 0, errors.New("对接码列表为空")
	}
	var raw struct {
		Affected int `json:"affected"`
	}
	body := map[string]any{"action": action, "codes": clean}
	if err := c.post(ctx, "/bindings/batch", body, &raw); err != nil {
		return 0, err
	}
	c.flushCache()
	return raw.Affected, nil
}

// ---- HTTP 底层 ----

// apiEnvelope 统一响应信封。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, u, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, BaseURL+path, payload, out)
}

func (c *Client) do(ctx context.Context, method, rawURL string, payload []byte, out any) error {
	c.mu.Lock()
	token := c.session
	c.mu.Unlock()
	if token == "" {
		return ErrNoSession
	}

	var rdr io.Reader
	if payload != nil {
		rdr = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://h5.haozhuma.cn/")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	cli := c.HTTP
	if cli == nil {
		cli = newHTTPClient()
		c.HTTP = cli
	}
	to := c.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req = req.WithContext(ctx2)

	resp, err := cli.Do(req)
	if err != nil {
		c.markError(err)
		// 脱敏：*url.Error 会带完整 URL（虽然 token 在头不在 URL，统一处理）。
		return fmt.Errorf("网络错误：%w", redactURL(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		c.markError(err)
		return fmt.Errorf("读取响应失败：%w", err)
	}

	// 401/403 一律判定 token 失效，不去猜 body。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		c.expired = true
		c.lastError = ErrSessionExpired.Error()
		c.mu.Unlock()
		return ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("上游 HTTP %d：%s", resp.StatusCode, truncate(string(body), 160))
		c.markError(err)
		return err
	}

	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		err := fmt.Errorf("响应不是 JSON（HTTP %d）：%s", resp.StatusCode, truncate(string(body), 160))
		c.markError(err)
		return err
	}
	switch env.Code {
	case 0:
		// ok
	case 401, 403:
		c.mu.Lock()
		c.expired = true
		c.lastError = ErrSessionExpired.Error()
		c.mu.Unlock()
		return ErrSessionExpired
	default:
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "未知错误"
		}
		err := fmt.Errorf("%s（code=%d）", msg, env.Code)
		c.markError(err)
		return err
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			err := fmt.Errorf("响应解析失败：%w", err)
			c.markError(err)
			return err
		}
	}
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.lastError = ""
	c.mu.Unlock()
	return nil
}

func (c *Client) markError(err error) {
	c.mu.Lock()
	c.lastError = err.Error()
	c.mu.Unlock()
}

func (c *Client) cached(key string) (any, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil, false
	}
	return e.value, true
}

func (c *Client) store(key string, value any) {
	c.cacheMu.Lock()
	c.cache[key] = cacheEntry{at: time.Now(), value: value}
	c.cacheMu.Unlock()
}

func (c *Client) flushCache() {
	c.cacheMu.Lock()
	c.cache = make(map[string]cacheEntry)
	c.cacheMu.Unlock()
}

// newHTTPClient 连接池参数：网页端并发不高，保守配置即可。
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func redactURL(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if i := strings.Index(msg, "https://"); i >= 0 {
		rest := msg[i:]
		if j := strings.IndexAny(rest, " :"); j > 0 {
			return errors.New(msg[:i] + rest[:j] + " [URL 已脱敏]")
		}
	}
	return err
}
