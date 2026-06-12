package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An absent config file must still produce a fully working configuration;
// the README promises a no-config run works.
func TestConfigDefaults(t *testing.T) {
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenFGA.APIURL != "http://localhost:8080" {
		t.Errorf("api_url default: got %q", cfg.OpenFGA.APIURL)
	}
	if cfg.ModelFile != "model.json" {
		t.Errorf("model_file default: got %q", cfg.ModelFile)
	}
	if cfg.Seed.Cohorts == 0 || cfg.Seed.BatchSize == 0 || cfg.Seed.Writers == 0 {
		t.Errorf("seed defaults missing: %+v", cfg.Seed)
	}
	if cfg.Load.Concurrency == 0 || cfg.Load.Duration == 0 {
		t.Errorf("load defaults missing: %+v", cfg.Load)
	}
	if cfg.KeepStore {
		t.Error("keep_store must default to false: `all` cleans up after itself")
	}
	if _, ok := cfg.Pools["default"]; !ok {
		t.Error("default value pool missing")
	}
}

func TestConfigOverridesSurviveDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "model_file: my-model.json\nkeep_store: true\nseed:\n  cohorts: 11\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelFile != "my-model.json" || !cfg.KeepStore || cfg.Seed.Cohorts != 11 {
		t.Errorf("overrides lost: model_file=%q keep_store=%v cohorts=%d", cfg.ModelFile, cfg.KeepStore, cfg.Seed.Cohorts)
	}
	if cfg.Load.Concurrency == 0 {
		t.Error("defaults not applied alongside overrides")
	}
}

func TestPoolMaterialize(t *testing.T) {
	explicit := PoolConfig{Values: []string{"a", "b"}}
	if got := explicit.Materialize(); len(got) != 2 || got[0] != "a" {
		t.Errorf("explicit values: got %v", got)
	}
	generated := PoolConfig{Prefix: "scope-", Count: 3}
	want := []string{"scope-00", "scope-01", "scope-02"}
	got := generated.Materialize()
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("generated pool: got %v, want %v", got, want)
	}
}
