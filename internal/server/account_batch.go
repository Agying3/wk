package server

import (
	"net/http"
	"strings"
)

const maxAccountBatch = 100

type accountBatchRequest struct {
	UIDs   []string `json:"uids"`
	Action string   `json:"action"`
	Groups []string `json:"groups"`
	Mode   string   `json:"mode"`
}

type accountBatchRow struct {
	UID    string `json:"uid"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Local mutations only: no provider requests, credentials or destructive
// account deletion. Each request performs at most one persistence operation.
func (h *Handler) accountBatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req accountBatchRequest
	if err := jsonDecodeLimited(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式不正确"})
		return
	}
	if len(req.UIDs) == 0 || len(req.UIDs) > maxAccountBatch {
		writeJSON(w, 400, map[string]string{"error": "每批须选择 1–100 个账号"})
		return
	}
	switch req.Action {
	case "enable", "disable", "clear-cooldown", "groups":
	default:
		writeJSON(w, 400, map[string]string{"error": "不支持的批量操作"})
		return
	}
	uids := make([]string, 0, len(req.UIDs))
	seen := map[string]bool{}
	for _, uid := range req.UIDs {
		if strings.TrimSpace(uid) == "" || len(uid) > 200 {
			writeJSON(w, 400, map[string]string{"error": "账号 UID 无效"})
			return
		}
		if !seen[uid] {
			uids = append(uids, uid)
			seen[uid] = true
		}
	}
	if h.cfg.Pool == nil || (req.Action == "groups" && h.cfg.Groups == nil) {
		writeJSON(w, 503, map[string]string{"error": "账号或分组存储未启用"})
		return
	}
	h.accountAdminMu.Lock()
	defer h.accountAdminMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	rows := make([]accountBatchRow, 0, len(uids))
	regions := make(map[string]string, len(uids))
	for _, uid := range uids {
		row := accountBatchRow{UID: uid, Status: "success", Detail: "操作成功"}
		account := h.cfg.Pool.AuthByUID(uid)
		if account == nil {
			row.Status, row.Detail = "failed", "账号不存在"
		} else {
			regions[uid] = account.Region()
		}
		rows = append(rows, row)
	}
	stop := false
	if req.Action == "groups" {
		issues, err := h.cfg.Groups.ChangeAccounts(regions, req.Groups, req.Mode)
		for i := range rows {
			if rows[i].Status == "failed" {
				continue
			}
			if issue := issues[rows[i].UID]; issue != nil {
				rows[i].Status, rows[i].Detail = "failed", issue.Error()
			} else if err != nil {
				rows[i].Status, rows[i].Detail = "failed", err.Error()
			} else {
				rows[i].Detail = "分组已保存"
			}
		}
		stop = err != nil
	} else {
		changed := false
		for i := range rows {
			if rows[i].Status == "failed" {
				continue
			}
			uid := rows[i].UID
			ok := true
			switch req.Action {
			case "enable":
				ok = h.cfg.Pool.Enable(uid)
			case "disable":
				h.cfg.Pool.Disable(uid, "manual disabled")
			case "clear-cooldown":
				ok = h.cfg.Pool.ClearCooldown(uid)
			}
			if !ok {
				rows[i].Status, rows[i].Detail = "failed", "账号已不存在，请刷新核对"
			} else {
				changed = true
			}
		}
		if changed {
			if err := h.cfg.Pool.FlushWithError(); err != nil {
				stop = true
				for i := range rows {
					if rows[i].Status == "success" {
						rows[i].Status, rows[i].Detail = "failed", "内存状态已变更，但持久化失败，请刷新核对后再操作"
					}
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"results": rows, "stop": stop})
}
