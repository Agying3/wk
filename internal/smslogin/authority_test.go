package smslogin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSMSAccountLookupKeepsAuthority(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"account":{"uid":"u","nickname":"n","enterpriseId":"e","type":"ultimate","oneidAccountId":"oid","idp":"provider","departmentFullName":"department"}}}`)
	}))
	defer api.Close()
	s := &session{cliState: "fake-state", cliHTTP: api.Client()}
	a, err := s.lookupAccount(context.Background(), Endpoints{CLI: api.URL}, "fake-at")
	if err != nil || a.UID != "u" || a.EnterpriseID != "e" || a.AccountType != "ultimate" || a.OneIDAccountID != "oid" || a.IDP != "provider" || a.DepartmentFullName != "department" {
		t.Fatalf("account=%+v err=%v", a, err)
	}
}
