package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestExpertTasksUsePaginationAndUniqueRealEntries(t *testing.T) {
	fastGrowthActions(t)
	for _, tc := range []struct {
		kind string
		need int
		run  func(*Handler, *auth.Auth) (string, error)
	}{{"agent", 5, runExpertUse}, {"team", 3, runExpertTeamUse}} {
		t.Run(tc.kind, func(t *testing.T) {
			var mu sync.Mutex
			var pages []int
			var chats []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/portal/operation-platform/market/expert/list":
					var query struct {
						Page int    `json:"page"`
						Kind string `json:"expert_type"`
					}
					_ = json.NewDecoder(r.Body).Decode(&query)
					pages = append(pages, query.Page)
					if query.Kind != tc.kind {
						t.Errorf("kind=%q", query.Kind)
					}
					rows := make([]upstream.MarketExpert, upstream.MarketExpertPageSize)
					for i := range rows {
						id := "ex-first"
						if query.Page > 1 {
							id = fmt.Sprintf("ex-%d", i)
						}
						rows[i] = upstream.MarketExpert{ExpertID: id, ExpertType: tc.kind, Version: "2.0", DisplayNameZH: id}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": rows}})
				case "/v2/chat/completions":
					chats = append(chats, r.Header.Get("X-Expert-Id"))
					if (r.Header.Get("X-Expert-Team-Task") == "true") != (tc.kind == "team") {
						t.Error("team flag lost")
					}
					_, _ = fmt.Fprint(w, "data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[{\"delta\":{\"content\":\"2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				case "/v2/report":
					_, _ = fmt.Fprint(w, `{"code":0,"data":{}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
			h := &Handler{cfg: Config{Upstream: c}}
			if _, err := tc.run(h, &auth.Auth{UID: "fixture-uid", AccessToken: "fixture-token"}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(pages) != 2 || pages[0] != 1 || pages[1] != 2 || len(chats) != tc.need {
				t.Fatalf("pages=%v chats=%v", pages, chats)
			}
			seen := map[string]bool{}
			for _, id := range chats {
				if id == "" || seen[id] {
					t.Fatalf("empty or duplicate expert id=%q", id)
				}
				seen[id] = true
			}
		})
	}
}

func TestExpertLookupStopsWithoutExecutingPartialTask(t *testing.T) {
	for _, mode := range []string{"empty", "short", "repeat", "limit", "error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/portal/operation-platform/market/expert/list" {
					t.Errorf("partial task executed: %s", r.URL.Path)
				}
				calls++
				if mode == "error" {
					w.WriteHeader(401)
					_, _ = fmt.Fprint(w, `{"code":1}`)
					return
				}
				var rows []upstream.MarketExpert
				if mode != "empty" {
					rows = make([]upstream.MarketExpert, upstream.MarketExpertPageSize)
					for i := range rows {
						id := "ex-same"
						if mode == "limit" {
							id = fmt.Sprintf("ex-%d-%d", calls, i)
						}
						rows[i] = upstream.MarketExpert{ExpertID: id, ExpertType: "agent"}
					}
					if mode == "short" {
						rows = rows[:1]
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": rows}})
			}))
			defer srv.Close()
			c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
			if mode == "limit" {
				_, err := taskMarketExperts(c, &auth.Auth{}, "agent", 1, func(upstream.MarketExpert) bool { return false })
				if err == nil || calls != taskExpertMaxPages {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			} else {
				_, err := runExpertUse(&Handler{cfg: Config{Upstream: c}}, &auth.Auth{})
				want := 1
				if mode == "repeat" {
					want = 2
				}
				if err == nil || calls != want {
					t.Fatalf("calls=%d want=%d err=%v", calls, want, err)
				}
			}
		})
	}
}

func TestLighthouseSearchUsesActualMetadataAndFallsBackToPaging(t *testing.T) {
	for _, searchHit := range []bool{true, false} {
		t.Run(fmt.Sprint(searchHit), func(t *testing.T) {
			searchCalls, pageCalls := 0, 0
			want := upstream.MarketExpert{ExpertID: "ex_2cvvUZQhDyeJ", ExpertType: "agent", Version: "9.0", IsCloud: 1, NeedPay: 1, DisplayNameZH: "腾讯轻量云专家"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var query struct {
					Keyword string `json:"keyword"`
					Page    int    `json:"page"`
				}
				_ = json.NewDecoder(r.Body).Decode(&query)
				var rows []upstream.MarketExpert
				if query.Keyword != "" {
					searchCalls++
					if query.Keyword != "腾讯轻量云" || r.Header.Get("X-Language") != "zh-CN" {
						t.Error("search shape mismatch")
					}
					if searchHit {
						rows = append(rows, want)
					}
				} else {
					pageCalls++
					rows = make([]upstream.MarketExpert, upstream.MarketExpertPageSize)
					for i := range rows {
						rows[i].ExpertID = fmt.Sprintf("ex-%d", i)
					}
					if query.Page == 2 {
						rows[0] = want
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": rows}})
			}))
			defer srv.Close()
			c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
			got, err := taskLighthouseExpert(c, &auth.Auth{})
			if err != nil {
				t.Fatal(err)
			}
			wantPages := 0
			if !searchHit {
				wantPages = 2
			}
			if searchCalls != 1 || pageCalls != wantPages || got.Version != want.Version || got.IsCloud != 1 || got.NeedPay != 1 {
				t.Fatalf("search=%d pages=%d expert=%+v", searchCalls, pageCalls, got)
			}
		})
	}
}
