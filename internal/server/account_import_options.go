package server

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
)

func normalizeImportOptions(req *accountImportRequest, doc *accountTransferDocument) error {
	if req.GroupMode == "" {
		req.GroupMode = "source"
	}
	switch req.GroupMode {
	case "source":
		if len(req.Groups) > 0 {
			return errors.New("选择目标分组时，请使用替换或追加模式")
		}
	case "replace", "append":
		if len(req.Groups) == 0 || len(req.Groups) > 1000 {
			return errors.New("请选择 1–1000 个目标分组")
		}
	default:
		return errors.New("导入分组模式无效")
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, name := range req.Groups {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("目标分组不能为空")
		}
		if !seen[name] {
			clean = append(clean, name)
			seen[name] = true
		}
	}
	req.Groups = clean
	req.Nickname = strings.TrimSpace(req.Nickname)
	if utf8.RuneCountInString(req.Nickname) > 128 || strings.IndexFunc(req.Nickname, unicode.IsControl) >= 0 {
		return errors.New("昵称最多 128 个字符，且不能包含控制字符")
	}
	if req.Nickname == "" {
		return nil
	}
	if len(doc.Accounts) != 1 {
		return errors.New("昵称覆盖仅支持单账号导入")
	}
	var credential map[string]json.RawMessage
	if err := json.Unmarshal(doc.Accounts[0].Credential, &credential); err != nil {
		return errors.New("账号凭据无效")
	}
	nickname, _ := json.Marshal(req.Nickname)
	if raw, nested := credential["auth"]; nested && len(raw) > 0 {
		var account map[string]json.RawMessage
		if json.Unmarshal(credential["account"], &account) != nil || account == nil {
			account = map[string]json.RawMessage{}
		}
		account["nickname"] = nickname
		credential["account"], _ = json.Marshal(account)
	} else {
		credential["nickname"] = nickname
	}
	doc.Accounts[0].Credential, _ = json.Marshal(credential)
	return validateTransferDocument(*doc)
}

// Explicit targets are strict: an invalid or incompatible choice is shown in
// preview and cannot silently send the account to default or a different group.
func (h *Handler) resolveImportGroups(a *auth.Auth, source []string, req accountImportRequest) ([]string, string) {
	if req.GroupMode == "source" {
		groups := h.importGroups(a, source)
		if len(groups) == 0 {
			return nil, "无兼容目标分组，请先配置 default 区域或来源同名分组"
		}
		return groups, ""
	}
	available := map[string]string{}
	if h.cfg.Groups == nil {
		available["default"] = "all"
	} else {
		for _, g := range h.cfg.Groups.ListDetailed() {
			available[g.Name] = g.Region
		}
	}
	for _, name := range req.Groups {
		region, exists := available[name]
		if !exists {
			return nil, "所选目标分组不存在，请刷新分组并重新预览"
		}
		if region != "all" && region != a.Region() {
			return nil, "所选目标分组与账号区域不兼容，请重新选择"
		}
	}
	out := []string{}
	if req.GroupMode == "append" {
		out = h.importGroups(a, source)
	}
	seen := map[string]bool{}
	for _, name := range out {
		seen[name] = true
	}
	for _, name := range req.Groups {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out, ""
}
