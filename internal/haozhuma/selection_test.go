package haozhuma

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeISP(t *testing.T) {
	for _, value := range []string{"", "1", "5", "9", "14", "16", " 1, 5,9,14,16 "} {
		if _, err := NormalizeISP(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"2", "3", "4", "15", "17", "1,1", "1,,5", "移动"} {
		if _, err := NormalizeISP(value); err == nil {
			t.Fatalf("accepted invalid ISP %q", value)
		}
	}
	if got := legacyISP(" 1,2,3,5 "); got != "1,5,9" {
		t.Fatalf("legacy selection: %q", got)
	}
}

func TestMonitorSelectionDoesNotBroaden(t *testing.T) {
	for _, message := range []string{"没有取到号码，请重新尝试", "没有这个[52283-ONLY]专属码"} {
		t.Run(message, func(t *testing.T) {
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("uid") != "52283-ONLY" {
					t.Error("monitor changed supplier code")
				}
				seen = append(seen, q.Get("isp"))
				fmt.Fprintf(w, `{"code":"-1","msg":%q}`, message)
			}))
			defer srv.Close()
			c := New("fixture")
			c.Base = srv.URL
			c.ISP = "1"
			c.SetUID("52283-MANUAL")
			isp := "5,9,14,16"
			ctx := WithPhoneSelection(context.Background(), PhoneSelection{UID: "52283-ONLY", ISP: &isp})
			if _, err := c.GetPhone(ctx, "52283"); err == nil {
				t.Fatal("expected no inventory")
			}
			want := isp
			if strings.Contains(message, "专属码") {
				want = "5"
			}
			if got := strings.Join(seen, ","); got != want {
				t.Fatalf("selection broadened: %q", got)
			}
			if c.ISP != "1" || c.UID() != "52283-MANUAL" {
				t.Fatal("monitor changed manual config")
			}
		})
	}
}

func TestMonitorExplicitUnrestrictedIgnoresManualISP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("isp") {
			t.Error("manual ISP leaked into monitor")
		}
		if got := r.URL.Query().Get("Province"); got != "44" {
			t.Errorf("province=%q", got)
		}
		if got := r.URL.Query().Get("ascription"); got != "2" {
			t.Errorf("ascription=%q, want real-card code 2", got)
		}
		fmt.Fprint(w, `{"code":"0","phone":"17000000001"}`)
	}))
	defer srv.Close()
	c := New("fixture")
	c.Base = srv.URL
	c.ISP = "1"
	isp := ""
	ctx := WithPhoneSelection(context.Background(), PhoneSelection{UID: "52283-A", ISP: &isp, Province: "44", CardType: "2"})
	if _, err := c.GetPhone(ctx, "52283"); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeProvince(t *testing.T) {
	for _, value := range []string{"", "11", "21", "31", "44", "50", "65", " 33 "} {
		if _, err := NormalizeProvince(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"0", "10", "20", "66", "广东", "44,11"} {
		if _, err := NormalizeProvince(value); err == nil {
			t.Fatalf("accepted invalid province %q", value)
		}
	}
}

func TestNormalizeCardType(t *testing.T) {
	for _, value := range []string{"", "1", "2", " 2 "} {
		if _, err := NormalizeCardType(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"0", "3", "real", "实卡"} {
		if _, err := NormalizeCardType(value); err == nil {
			t.Fatalf("accepted card type %q", value)
		}
	}
}
