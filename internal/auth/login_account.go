package auth

import (
	"encoding/json"
	"fmt"
)

// LoginAccount contains only provider-returned account data. It does not
// synthesize authority fields absent from an older login response.
type LoginAccount struct {
	UID, EnterpriseID, Nickname string
	Authority
}

func ParseLoginAccount(raw []byte) (LoginAccount, error) {
	type core struct {
		UID          string `json:"uid"`
		UserID       string `json:"userId"`
		ID           string `json:"id"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Name         string `json:"name"`
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return LoginAccount{}, fmt.Errorf("invalid login account response")
	}
	var top core
	if json.Unmarshal(raw, &top) != nil {
		return LoginAccount{}, fmt.Errorf("invalid login account fields")
	}
	first := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	selected := root
	uid := first(top.UID, top.UserID, top.ID)
	var nested core
	var nestedFields map[string]json.RawMessage
	if root["account"] != nil {
		if json.Unmarshal(root["account"], &nestedFields) != nil || json.Unmarshal(root["account"], &nested) != nil {
			return LoginAccount{}, fmt.Errorf("invalid nested login account")
		}
		if uid == "" {
			top, selected = nested, nestedFields
			uid = first(top.UID, top.UserID, top.ID)
		} else if nestedUID := first(nested.UID, nested.UserID, nested.ID); nestedUID == "" || nestedUID == uid {
			top.EnterpriseID = first(top.EnterpriseID, nested.EnterpriseID)
			top.Nickname = first(top.Nickname, top.Name, nested.Nickname, nested.Name)
		} else {
			nestedFields = nil
		}
	}
	if uid == "" {
		return LoginAccount{}, fmt.Errorf("login account missing uid")
	}
	out := LoginAccount{UID: uid, EnterpriseID: top.EnterpriseID, Nickname: first(top.Nickname, top.Name)}
	for name, target := range map[string]*string{"type": &out.AccountType, "oneidAccountId": &out.OneIDAccountID, "idp": &out.IDP, "departmentFullName": &out.DepartmentFullName} {
		v := selected[name]
		if v == nil {
			v = nestedFields[name]
		}
		if v != nil {
			_ = json.Unmarshal(v, target)
		}
	}
	return out, nil
}
