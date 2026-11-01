package auth

import "testing"

func TestLoginAccountAuthoritySources(t *testing.T) {
	for _, raw := range []string{
		`{"uid":"u","enterpriseId":"e","nickname":"n","type":"ultimate","oneidAccountId":"oid","idp":"idp","departmentFullName":"department"}`,
		`{"account":{"userId":"u","enterpriseId":"e","name":"n","type":"ultimate","oneidAccountId":"oid","idp":"idp","departmentFullName":"department"}}`,
		`{"uid":"u","account":{"uid":"u","enterpriseId":"e","name":"n","type":"ultimate","oneidAccountId":"oid","idp":"idp","departmentFullName":"department"}}`,
	} {
		a, err := ParseLoginAccount([]byte(raw))
		if err != nil || a.UID != "u" || a.EnterpriseID != "e" || a.Nickname != "n" || a.AccountType != "ultimate" || a.OneIDAccountID != "oid" || a.IDP != "idp" || a.DepartmentFullName != "department" {
			t.Fatalf("account=%+v err=%v", a, err)
		}
	}
	a, err := ParseLoginAccount([]byte(`{"uid":"u","account":{"uid":"other","type":"ultimate","enterpriseId":"other-e"}}`))
	if err != nil || a.AccountType != "" || a.EnterpriseID != "" {
		t.Fatalf("mixed identities: %+v %v", a, err)
	}
	legacy, err := ParseLoginAccount([]byte(`{"uid":"u","type":{"legacy":true}}`))
	if err != nil || legacy.AccountType != "" {
		t.Fatalf("legacy extension was not tolerated: %+v %v", legacy, err)
	}
}
