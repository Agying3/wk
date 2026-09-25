package groups

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegionPersistence 区域 sidecar 的持久化与 legacy 兜底。
func TestRegionPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// legacy：未设置区域的分组 = all。
	if r := s.RegionOf(DefaultGroup); r != RegionAll {
		t.Fatalf("default region = %q, want all", r)
	}
	if err := s.CreateWithRegion("cn-vip", RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWithRegion("bad", "mars"); err == nil {
		t.Fatal("invalid region must fail")
	}
	if err := s.SetGroupRegion(DefaultGroup, RegionGlobal); err != nil {
		t.Fatalf("set default region: %v", err)
	}
	if err := s.SetGroupRegion("nope", RegionCN); err == nil {
		t.Fatal("unknown group must fail")
	}
	// 重开持久化正确。
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := s2.RegionOf("cn-vip"); r != RegionCN {
		t.Fatalf("cn-vip region = %q, want cn", r)
	}
	if r := s2.RegionOf(DefaultGroup); r != RegionGlobal {
		t.Fatalf("default region = %q, want global", r)
	}
	if r := s2.RegionOf("unknown"); r != RegionAll {
		t.Fatalf("unknown region = %q, want all", r)
	}
}

// TestRegionMigrateOnRenameAndDelete 改名迁移区域/绑定，删组清理两处条目。
func TestRegionMigrateOnRenameAndDelete(t *testing.T) {
	s := newStore(t)
	if err := s.CreateWithRegion("vip", RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyBindings("vip", []string{"pg_a", "pg_b", "pg_a", " "}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("vip", "premium"); err != nil {
		t.Fatal(err)
	}
	if r := s.RegionOf("vip"); r != RegionAll {
		t.Fatalf("old name region = %q, want all(unknown)", r)
	}
	if r := s.RegionOf("premium"); r != RegionCN {
		t.Fatalf("renamed region = %q, want cn", r)
	}
	if b := s.ProxyBindingsOf("premium"); len(b) != 2 || b[0] != "pg_a" || b[1] != "pg_b" {
		t.Fatalf("renamed bindings = %v, want [pg_a pg_b]", b)
	}
	// 删除后两处条目都没了。
	if err := s.Delete("premium"); err != nil {
		t.Fatal(err)
	}
	if b := s.ProxyBindingsOf("premium"); b != nil {
		t.Fatalf("deleted group bindings = %v, want nil", b)
	}
	snap := s.SnapshotProxyBindings()
	if _, ok := snap["premium"]; ok {
		t.Fatal("snapshot must not contain deleted group")
	}
}

// TestProxyBindingsCRUD 绑定的设置/查询/引用清理。
func TestProxyBindingsCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWithRegion("g1", RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyBindings("nope", []string{"pg_x"}); err == nil {
		t.Fatal("unknown group must fail")
	}
	if err := s.SetProxyBindings("g1", []string{"pg_1", "pg_2"}); err != nil {
		t.Fatal(err)
	}
	if b := s.ProxyBindingsOf("g1"); len(b) != 2 {
		t.Fatalf("bindings = %v", b)
	}
	// 清空。
	if err := s.SetProxyBindings("g1", nil); err != nil {
		t.Fatal(err)
	}
	if b := s.ProxyBindingsOf("g1"); len(b) != 0 {
		t.Fatalf("after clear = %v, want empty", b)
	}
	// 重新设置后清理引用。
	if err := s.SetProxyBindings("g1", []string{"pg_1", "pg_2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWithRegion("g2", RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyBindings("g2", []string{"pg_2"}); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveProxyGroupRef("pg_2"); n != 2 {
		t.Fatalf("affected = %d, want 2", n)
	}
	if b := s.ProxyBindingsOf("g1"); len(b) != 1 || b[0] != "pg_1" {
		t.Fatalf("g1 after ref removal = %v", b)
	}
	if b := s.ProxyBindingsOf("g2"); len(b) != 0 {
		t.Fatalf("g2 after ref removal = %v", b)
	}
	// 持久化：重开后引用清理结果保持。
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b := s2.ProxyBindingsOf("g1"); len(b) != 1 || b[0] != "pg_1" {
		t.Fatalf("reloaded g1 = %v", b)
	}
}

// TestRegisteredGroups 显式登记查询：与 AccountGroups 的 default 回落不同，
// RegisteredGroups 未登记返回 nil（供代理绑定解析用）。
func TestRegisteredGroups(t *testing.T) {
	s := newStore(t)
	if err := s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountGroups("u1", []string{"vip", DefaultGroup}); err != nil {
		t.Fatal(err)
	}
	if g := s.RegisteredGroups("u1"); len(g) != 2 {
		t.Fatalf("registered = %v", g)
	}
	if g := s.RegisteredGroups("unknown"); g != nil {
		t.Fatalf("unknown registered = %v, want nil", g)
	}
	if g := s.AccountGroups("unknown"); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("unknown fallback = %v, want [default]", g)
	}
}

// TestMigrateUnregisteredRegionFilter 区域过滤迁移：default=all 全登记；
// default=cn 时 global 账号保持未登记。
func TestMigrateUnregisteredRegionFilter(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// default = all：两种区域都登记。
	if n := s.MigrateUnregistered([]string{"u1", "u2"}, func(string) string { return "global" }); n != 2 {
		t.Fatalf("migrated = %d, want 2", n)
	}
	// default = cn：cn 登记成功。
	if err := s.SetGroupRegion(DefaultGroup, RegionCN); err != nil {
		t.Fatal(err)
	}
	if n := s.MigrateUnregistered([]string{"u3"}, func(string) string { return "cn" }); n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}
	// global 账号不进 default。
	if n := s.MigrateUnregistered([]string{"u4"}, func(string) string { return "global" }); n != 0 {
		t.Fatalf("migrated = %d, want 0 (region mismatch)", n)
	}
	if g := s.AccountGroups("u4"); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("u4 fallback = %v (未登记，读路径回落 default)", g)
	}
	if g := s.RegisteredGroups("u4"); g != nil {
		t.Fatalf("u4 registered = %v, want nil", g)
	}
	_ = os.Remove(filepath.Join(dir, "groups.json"))
}
