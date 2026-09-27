package groups

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeedSurvivesGroupCRUD 复现服务器现象：default 种子绑定写入后，
// 建组/删组会不会把绑定弄丢（groups 层面的回归测试）。
func TestSeedSurvivesGroupCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 种子
	if err := s.SetProxyBindings(DefaultGroup, []string{"pg_default"}); err != nil {
		t.Fatal(err)
	}
	// 建组 → 绑定 → 删组（模拟 cn-test 流程）
	if err := s.CreateWithRegion("cn-test", RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyBindings("cn-test", []string{"pg_default"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("cn-test"); err != nil {
		t.Fatal(err)
	}
	// default 绑定必须还在。
	if b := s.ProxyBindingsOf(DefaultGroup); len(b) != 1 || b[0] != "pg_default" {
		t.Fatalf("default binding lost after CRUD: %v", b)
	}
	// 落盘内容必须包含 default。
	raw, err := os.ReadFile(filepath.Join(dir, "proxy-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("file: %s", raw)
	if !contains(string(raw), "pg_default") {
		t.Fatalf("file content lost default binding: %s", raw)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
