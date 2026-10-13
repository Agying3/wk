package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCacheRoutingDefaultsAndExplicitDisable(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.CacheRouting.Enabled || c.CacheWaitTimeout != 5*time.Second || c.CacheRouting.MaxPendingTotal != 64 {
		t.Fatal("bad routing defaults")
	}
	if !c.CacheRouting.SessionProtection || !c.CacheRouting.FairQueue || c.ActiveLeaseMin != 5*time.Second || c.ActiveLeaseMax != 15*time.Second || c.QueueAging != 2*time.Second || c.CacheRouting.ReservedSlots != 1 || c.CacheRouting.MaxPendingPerSession != 4 {
		t.Fatal("bad admission defaults")
	}
	if err := json.Unmarshal([]byte(`{"cache_routing":{"enabled":false,"session_protection":false,"fair_queue":false,"max_cache_loss":0}}`), c); err != nil {
		t.Fatal(err)
	}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.CacheRouting.Enabled || c.CacheRouting.SessionProtection || c.CacheRouting.FairQueue || c.CacheRouting.MaxCacheLoss != 0 {
		t.Fatal("explicit zero/disable was ignored")
	}
}

func TestCacheRoutingRejectsInvalidConfig(t *testing.T) {
	for _, body := range []string{
		`{"cache_routing":{"wait_timeout":"-1s"}}`,
		`{"cache_routing":{"prefix_ttl":"broken"}}`,
		`{"cache_routing":{"max_pending_total":0}}`,
		`{"cache_routing":{"max_candidates":17}}`,
		`{"cache_routing":{"max_cache_loss":1.1}}`,
		`{"cache_routing":{"initial_prefill_tokens_per_second":-1}}`,
		`{"cache_routing":{"active_lease_min":"0s"}}`,
		`{"cache_routing":{"active_lease_max":"2s"}}`,
		`{"cache_routing":{"active_lease_max":"2m"}}`,
		`{"cache_routing":{"reserved_slots":0}}`,
		`{"cache_routing":{"queue_aging":"0s"}}`,
		`{"cache_routing":{"max_pending_per_session":0}}`,
		`{"cache_routing":{"max_pending_per_session":65}}`,
	} {
		c := Default()
		if err := json.Unmarshal([]byte(body), c); err != nil {
			t.Fatal(err)
		}
		if c.normalize() == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
