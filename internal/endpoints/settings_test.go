package endpoints

import "testing"

func TestEndpointProfilesAndOverrides(t *testing.T) {
	legacy, err := (Settings{}).Resolve()
	if err != nil || legacy.ChatBaseCN != LegacyChatCN || legacy.BillingBaseCN != LegacyBillingCN {
		t.Fatalf("legacy=%+v %v", legacy, err)
	}
	product, err := (Settings{EndpointProfile: "product"}).Resolve()
	if err != nil || product.ChatBaseCN != ProductCN || product.BillingBaseCN != ProductCN || product.ChatBaseGlobal != Global {
		t.Fatalf("product=%+v %v", product, err)
	}
	override, err := (Settings{EndpointProfile: "product", ChatBaseCN: "http://127.0.0.1:1234/prefix/"}).Resolve()
	if err != nil || override.ChatBaseCN != "http://127.0.0.1:1234/prefix" {
		t.Fatalf("override=%+v %v", override, err)
	}
	for _, base := range []string{"file:///tmp/x", "https://user:secret@example.test", "https://example.test?token=x", "https://example.test/#fragment"} {
		if _, err := (Settings{ChatBaseCN: base}).Resolve(); err == nil {
			t.Fatalf("accepted invalid endpoint")
		}
	}
}
