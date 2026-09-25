package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/groups"
)

// —— 分组管理（含区域 + 代理分组绑定） ——

// adminGroups GET 列表 / POST 新建（region 必填：cn|global）。
func (h *Handler) adminGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		// 附带每组的账号数与密钥数，管理页直接可渲染。
		own := h.cfg.Groups.SnapshotAccountGroups()
		counts := map[string]int{}
		for _, gs := range own {
			for _, g := range gs {
				counts[g]++
			}
		}
		keyCounts := map[string]int{}
		for _, k := range h.cfg.Groups.ListKeys() {
			if k.Group != "" {
				keyCounts[k.Group]++
			}
		}
		items := make([]map[string]any, 0)
		for _, g := range h.cfg.Groups.ListDetailed() {
			items = append(items, map[string]any{
				"name":              g.Name,
				"default":           g.Default,
				"region":            g.Region,
				"accounts":          counts[g.Name],
				"keys":              keyCounts[g.Name],
				"proxy_bindings":    len(g.ProxyGroups),
				"proxy_binding_ids": g.ProxyGroups,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": items})
	case http.MethodPost:
		var req struct {
			Name   string `json:"name"`
			Region string `json:"region"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if req.Region != groups.RegionCN && req.Region != groups.RegionGlobal {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新建分组必须指定区域（cn 或 global）"})
			return
		}
		if err := h.cfg.Groups.CreateWithRegion(req.Name, req.Region); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name, "region": req.Region})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// adminGroup 单分组：PUT 改名/改区域 / DELETE 删除。
func (h *Handler) adminGroup(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name   string `json:"name"`
			Region string `json:"region"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		// 改区域（default 也可改区域；名字不可改）。
		if req.Region != "" {
			if err := h.setGroupRegionChecked(name, req.Region); err != nil {
				writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
				return
			}
		}
		if req.Name != "" && req.Name != name {
			if err := h.cfg.Groups.Rename(name, req.Name); err != nil {
				writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := h.cfg.Groups.Delete(name); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// setGroupRegionChecked 改分组区域（先校验成员账号与既有绑定）。
func (h *Handler) setGroupRegionChecked(name, region string) error {
	if region != groups.RegionCN && region != groups.RegionGlobal && region != groups.RegionAll {
		return fmt.Errorf("分组区域不合法（cn/global/all）")
	}
	if h.cfg.Groups == nil {
		return errors.New("分组存储未启用")
	}
	// 组内成员账号区域必须全部兼容（all 时任意；cn 组不能装 global 账号）。
	if h.cfg.Pool != nil && region != groups.RegionAll {
		var violating []string
		for uid := range h.cfg.Groups.AccountsInGroup(name) {
			a := h.cfg.Pool.AuthByUID(uid)
			if a == nil {
				continue
			}
			if !groups.RegionCompatible(region, a.Region()) {
				violating = append(violating, uid)
			}
		}
		if len(violating) > 0 {
			return fmt.Errorf("分组 %s 内有 %d 个区域不匹配的账号（%s...），请先移出", name, len(violating), strings.Join(firstN(violating, 3), ", "))
		}
	}
	// 既有代理分组绑定必须与新区域兼容（组为 all 时均可；否则须同区）。
	if region != groups.RegionAll && h.cfg.ReqProxy != nil {
		binds := h.cfg.Groups.ProxyBindingsOf(name)
		var bad []string
		for _, pgID := range binds {
			for _, g := range h.cfg.ReqProxy.ProxyGroupsSnapshot() {
				if g.ID == pgID && g.Region != region {
					bad = append(bad, fmt.Sprintf("%s(%s)", g.Name, g.Region))
				}
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("分组 %s 绑定的代理分组区域不匹配：%s；请先解绑", name, strings.Join(bad, ", "))
		}
	}
	return h.cfg.Groups.SetGroupRegion(name, region)
}

// firstN 取前 n 个（展示用）。
func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// adminAccountGroups GET 查 / PUT 改账号归属（带区域校验）。
func (h *Handler) adminAccountGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	uid := r.PathValue("uid")
	if h.cfg.Pool == nil || h.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "groups": h.cfg.Groups.AccountGroups(uid)})
	case http.MethodPut:
		var req struct {
			Groups []string `json:"groups"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.setAccountGroupsChecked(uid, req.Groups); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": h.cfg.Groups.AccountGroups(uid)})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// setAccountGroupsChecked 账号入组（带区域校验）：
// 每个目标组的区域必须兼容账号区域（all 或同区）。
func (h *Handler) setAccountGroupsChecked(uid string, gs []string) error {
	if h.cfg.Pool == nil {
		return errors.New("账号池未启用")
	}
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return errors.New("账号不存在")
	}
	region := a.Region()
	for _, g := range gs {
		gr := h.cfg.Groups.RegionOf(g)
		if !groups.RegionCompatible(gr, region) {
			return fmt.Errorf("账号 %s 是 %s 区，不能加入 %s 区分组 %q", uid, region, gr, g)
		}
	}
	return h.cfg.Groups.SetAccountGroups(uid, gs)
}

// adminGroupProxyBindings GET 查 / PUT 改账号分组绑定的代理分组。
func (h *Handler) adminGroupProxyBindings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "proxy_groups": h.cfg.Groups.ProxyBindingsOf(name)})
	case http.MethodPut:
		var req struct {
			ProxyGroups []string `json:"proxy_groups"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		// 校验：每个 pgID 必须存在且区域与账号分组兼容（all 时任意）。
		groupRegion := h.cfg.Groups.RegionOf(name)
		if h.cfg.ReqProxy != nil {
			known := map[string]string{}
			for _, g := range h.cfg.ReqProxy.ProxyGroupsSnapshot() {
				known[g.ID] = g.Region
			}
			for _, pgID := range req.ProxyGroups {
				pgr, ok := known[pgID]
				if !ok {
					returnJSONErr(w, http.StatusBadRequest, fmt.Sprintf("代理分组 %s 不存在", pgID))
					return
				}
				if !groups.RegionCompatible(groupRegion, pgr) {
					returnJSONErr(w, http.StatusBadRequest,
						fmt.Sprintf("代理分组 %s 是 %s 区，不能绑定到 %s 区的账号分组 %q", pgID, pgr, groupRegion, name))
					return
				}
			}
		}
		if err := h.cfg.Groups.SetProxyBindings(name, req.ProxyGroups); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_groups": h.cfg.Groups.ProxyBindingsOf(name)})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// returnJSONErr 统一错误 JSON 输出小助手。
func returnJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// adminAutoDetectRegions 迁移辅助：按组内成员账号区域自动识别。
// 纯 CN 组→cn；纯 global 组→global；混合/空组跳过。返回每组的识别结果。
func (h *Handler) adminAutoDetectRegions(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if h.cfg.Pool == nil {
		returnJSONErr(w, http.StatusServiceUnavailable, "账号池未启用")
		return
	}
	type result struct {
		Name   string `json:"name"`
		Region string `json:"region"`
		Action string `json:"action"` // set|skipped
		Reason string `json:"reason,omitempty"`
	}
	var out []result
	for _, g := range h.cfg.Groups.ListDetailed() {
		if g.Region != groups.RegionAll {
			out = append(out, result{Name: g.Name, Region: g.Region, Action: "skipped", Reason: "已设置"})
			continue
		}
		cn, global := 0, 0
		for uid := range h.cfg.Groups.AccountsInGroup(g.Name) {
			a := h.cfg.Pool.AuthByUID(uid)
			if a == nil {
				continue
			}
			switch a.Region() {
			case groups.RegionCN:
				cn++
			case groups.RegionGlobal:
				global++
			}
		}
		switch {
		case cn > 0 && global == 0:
			if err := h.setGroupRegionChecked(g.Name, groups.RegionCN); err != nil {
				out = append(out, result{Name: g.Name, Action: "skipped", Reason: err.Error()})
				continue
			}
			out = append(out, result{Name: g.Name, Region: groups.RegionCN, Action: "set"})
		case global > 0 && cn == 0:
			if err := h.setGroupRegionChecked(g.Name, groups.RegionGlobal); err != nil {
				out = append(out, result{Name: g.Name, Action: "skipped", Reason: err.Error()})
				continue
			}
			out = append(out, result{Name: g.Name, Region: groups.RegionGlobal, Action: "set"})
		case cn == 0 && global == 0:
			out = append(out, result{Name: g.Name, Action: "skipped", Reason: "组内无账号"})
		default:
			out = append(out, result{Name: g.Name, Action: "skipped", Reason: "组内 CN/国际账号混合"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": out})
}

func statusForGroupErr(err error) int {
	switch {
	case errors.Is(err, groups.ErrImmutable):
		return http.StatusForbidden
	case errors.Is(err, groups.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, groups.ErrGroupInUse):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// —— 密钥管理 ——

// adminKeys GET 列表（脱敏）/ POST 创建（完整密钥只在响应里出现一次）。
func (h *Handler) adminKeys(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"keys": h.cfg.Groups.ListKeys()})
	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if req.Group == "" {
			req.Group = groups.DefaultGroup
		}
		k, err := h.cfg.Groups.CreateKey(req.Name, req.Group)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": k.ID, "key": k.Key, "name": k.Name, "group": k.Group,
			"note": "完整密钥仅此一次回显，请立即保存",
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// adminKey 单密钥：PUT 改备注/改绑分组 / DELETE 删除。
func (h *Handler) adminKey(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.cfg.Groups.UpdateKey(id, req.Name, req.Group); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := h.cfg.Groups.DeleteKey(id); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// jsonDecodeLimited 限长解码 JSON body。
func jsonDecodeLimited(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(v)
}
