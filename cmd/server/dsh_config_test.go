package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDSHCompatConfigAndEnvironment(t *testing.T) {
	t.Setenv("WB2A_DSH_COMPAT", "")
	if Default().Features.DSHCompat {
		t.Fatal("DSH compatibility must default to disabled")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"features":{"dsh_compat":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.DSHCompat {
		t.Fatal("file config not loaded")
	}
	t.Setenv("WB2A_DSH_COMPAT", "false")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.DSHCompat {
		t.Fatal("environment did not override file config")
	}
	t.Setenv("WB2A_DSH_COMPAT", "true")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.DSHCompat {
		t.Fatal("environment enable not loaded")
	}
}
