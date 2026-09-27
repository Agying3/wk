// 代理分组解析器 + reqproxy 代理分组管理端点。
//
// resolver：账号归属分组（显式登记）→ 各分组绑定的代理分组并集。
// 语义（D4）：任一分组无绑定时该账号不受限（nil）；仅当全部归属分组都
// 绑定了代理分组时才限制为并集。失效 pgID 容忍（并集里的死 ID 自然无节点）。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/reqproxy"
)

// errInvalidRegion 区域取值不合法。
var errInvalidRegion = errors.New("代理分组区域必须是 cn、global 或 all")

// regionConflictError 区域变更/绑定与既有引用冲突（带名单的可读错误）。
type regionConflictError struct {
	groupKind string   // 冲突的引用侧类型（账号分组）
	groups    []string // 冲突名单
	target    string   // 目标区域
}

func (e *regionConflictError) Error() string {
	return fmt.Sprintf("有 %d 个%s与其区域（%s）不兼容（%v），请先调整",
		len(e.groups), e.groupKind, e.target, e.groups)
}

func itoa(n int) string { return strconv.Itoa(n) }

// proxyGroupResolver 账号 → 允许的代理分组（注入 reqproxy.Pool）。
type proxyGroupResolver struct {
	h *Handler
}

// AllowedProxyGroups 实现 reqproxy.GroupResolver。
//
// nil = 不限制；非 nil = 限制到该集合（空集合 = 全部受限，选不到已分组
// 节点）。
func (r proxyGroupResolver) AllowedProxyGroups(uid string) map[string]bool {
	gs := r.h.cfg.Groups.RegisteredGroups(uid)
	if len(gs) == 0 {
		return nil // 未登记：不受限（与 keys.json 选号只看归属表一致）
	}
	restricted := true
	union := map[string]bool{}
	for _, g := range gs {
		binds := r.h.cfg.Groups.ProxyBindingsOf(g)
		if len(binds) == 0 {
			restricted = false // 任一分组未绑定 → 不限制
			break
		}
		for _, pgID := range binds {
			union[pgID] = true
		}
	}
	if !restricted {
		return nil
	}
	return union
}

// reqproxyGroupsGet 代理分组列表（含节点数/订阅来源）。
func (h *Handler) reqproxyGroupsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	// 节点计数：按 group_id 聚合节点快照。
	nodeCount := map[string]int{}
	var ungrouped int
	for _, n := range h.cfg.ReqProxy.NodesSnapshot() {
		if n.GroupID == "" {
			ungrouped++
			continue
		}
		nodeCount[n.GroupID]++
	}
	out := make([]map[string]any, 0)
	for _, g := range h.cfg.ReqProxy.ProxyGroupsSnapshot() {
		item := map[string]any{
			"id": g.ID, "name": g.Name, "region": g.Region,
			"nodes":      nodeCount[g.ID],
			"created_at": g.CreatedAt,
			"is_default": g.ID == reqproxy.DefaultProxyGroupID,
		}
		if g.SubID != "" {
			item["sub_id"] = g.SubID
			// 订阅名（订阅可能已改过名，分组展示名是创建时的快照）。
			h.cfg.ReqProxy.View(func(st *reqproxy.State) {
				for _, s := range st.Subscriptions {
					if s.ID == g.SubID {
						item["sub_name"] = s.Name
					}
				}
			})
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "ungrouped_nodes": ungrouped})
}

// reqproxyGroupsPost 新建手动代理分组。
func (h *Handler) reqproxyGroupsPost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Name   string `json:"name"`
		Region string `json:"region"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		returnJSONErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	g, err := h.cfg.ReqProxy.CreateProxyGroup(req.Name, req.Region)
	if err != nil {
		returnJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": g.ID, "name": g.Name, "region": g.Region})
}

// reqproxyGroupPutDelete 单分组：PUT 改名/改区域，DELETE 删除（仅手动组）。
func (h *Handler) reqproxyGroupPutDelete(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name   string `json:"name"`
			Region string `json:"region"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			returnJSONErr(w, http.StatusBadRequest, "请求格式不正确")
			return
		}
		if req.Region != "" {
			if err := h.checkProxyGroupRegionChange(id, req.Region); err != nil {
				returnJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if err := h.cfg.ReqProxy.SetProxyGroupRegion(id, req.Region); err != nil {
				returnJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.Name != "" {
			if err := h.cfg.ReqProxy.RenameProxyGroup(id, req.Name); err != nil {
				returnJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		// 删除前解除账号分组绑定侧的引用（失效 ID 虽被解析器容忍，
		// 主动清理让管理页的绑定列表始终干净）。
		if h.cfg.Groups != nil {
			// 确认存在且非订阅专属组。
			for _, g := range h.cfg.ReqProxy.ProxyGroupsSnapshot() {
				if g.ID == id && g.SubID == "" {
					if n := h.cfg.Groups.RemoveProxyGroupRef(id); n > 0 {
						h.cfg.ReqProxy.Emit("info", "代理分组删除：已解除 "+itoa(n)+" 个账号分组的绑定引用")
					}
					break
				}
			}
		}
		if _, err := h.cfg.ReqProxy.DeleteProxyGroup(id); err != nil {
			returnJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		returnJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// checkProxyGroupRegionChange 代理分组区域变更的反向校验：
// 绑定了它的账号分组必须为 all 或同区，否则拒绝（并给出名单）。
// 代理分组改 all 天然兼容所有账号分组（RegionCompatible 对 all 恒真）。
func (h *Handler) checkProxyGroupRegionChange(pgID, newRegion string) error {
	if newRegion != groups.RegionCN && newRegion != groups.RegionGlobal && newRegion != groups.RegionAll {
		return errInvalidRegion
	}
	if h.cfg.Groups == nil {
		return nil
	}
	var bad []string
	for g, pgIDs := range h.cfg.Groups.SnapshotProxyBindings() {
		refs := false
		for _, id := range pgIDs {
			if id == pgID {
				refs = true
				break
			}
		}
		if !refs {
			continue
		}
		if gr := h.cfg.Groups.RegionOf(g); !groups.RegionCompatible(gr, newRegion) {
			bad = append(bad, g)
		}
	}
	if len(bad) > 0 {
		return &regionConflictError{groupKind: "账号分组", groups: bad, target: newRegion}
	}
	return nil
}

// reqproxyNodesGroupBatch 批量挪组（一次 rebuild；前端勾选多节点用）。
func (h *Handler) reqproxyNodesGroupBatch(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		returnJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		NodeIDs []string `json:"node_ids"`
		GroupID string   `json:"group_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		returnJSONErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if len(req.NodeIDs) == 0 {
		returnJSONErr(w, http.StatusBadRequest, "node_ids 不能为空")
		return
	}
	if len(req.NodeIDs) > 1000 {
		returnJSONErr(w, http.StatusBadRequest, "单次最多挪动 1000 个节点")
		return
	}
	moved, err := h.cfg.ReqProxy.SetManualNodesGroup(req.NodeIDs, req.GroupID)
	if err != nil {
		returnJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved})
}

// reqproxyNodeGroup 手动节点挪组。
func (h *Handler) reqproxyNodeGroup(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		returnJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	nodeID := r.PathValue("id")
	var req struct {
		GroupID string `json:"group_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		returnJSONErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if err := h.cfg.ReqProxy.SetManualNodeGroup(nodeID, req.GroupID); err != nil {
		returnJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
