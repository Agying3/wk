package upstream

import (
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// 专家列表：翻页只改 page，搜索多 keyword 与 X-Language（未搜索时不带）。
func TestMarketExpertListPagingAndSearch(t *testing.T) {
	var bodies []map[string]any
	var languages []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		languages = append(languages, r.Header.Get("X-Language"))
		return jsonResp(200, `{"code":0,"data":{"items":[{"expert_id":"ex_1","expert_type":"agent"}]}}`), nil
	})
	a := &auth.Auth{UID: "u", AccessToken: "at"}
	if _, err := c.MarketExpertListPage(a, "agent", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MarketExpertSearch(a, "agent", " 无畏契约拼豆专家 "); err != nil {
		t.Fatal(err)
	}
	if bodies[0]["page"] != float64(3) || bodies[0]["keyword"] != nil {
		t.Fatalf("page request=%v", bodies[0])
	}
	if languages[0] != "" {
		t.Fatalf("plain list must not send X-Language: %q", languages[0])
	}
	if bodies[1]["keyword"] != "无畏契约拼豆专家" || bodies[1]["page"] != float64(1) {
		t.Fatalf("search request=%v", bodies[1])
	}
	if languages[1] != "zh-CN" {
		t.Fatalf("search X-Language=%q", languages[1])
	}
}
