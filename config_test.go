package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	yaml := "model_file: my-model.json\nkeep_store: true\nseed:\n  cohorts: 11\ncontextual:\n  relations: [document#active_context]\n  attach_probability: 0.25\n"
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
	if len(cfg.Contextual.Relations) != 1 || cfg.Contextual.Relations[0] != "document#active_context" {
		t.Errorf("contextual relations lost: %+v", cfg.Contextual.Relations)
	}
	if cfg.Contextual.AttachProbability == nil || *cfg.Contextual.AttachProbability != 0.25 {
		t.Errorf("contextual attach_probability lost: %+v", cfg.Contextual.AttachProbability)
	}
}

func TestExplicitZeroConfigValuesSurviveDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `seed:
  wildcard_probability: 0
probe:
  cohort_bias: 0
  allowed_ratio: 0
  max_duplication: 0
load:
  warmup: 0s
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Seed.WildcardProb != 0 {
		t.Errorf("wildcard_probability = %v, want explicit zero", cfg.Seed.WildcardProb)
	}
	if cfg.Probe.CohortBias != 0 {
		t.Errorf("cohort_bias = %v, want explicit zero", cfg.Probe.CohortBias)
	}
	if cfg.Probe.AllowedRatio != 0 {
		t.Errorf("allowed_ratio = %v, want explicit zero", cfg.Probe.AllowedRatio)
	}
	if cfg.Probe.MaxDuplication != 0 {
		t.Errorf("max_duplication = %v, want explicit zero", cfg.Probe.MaxDuplication)
	}
	if cfg.Load.Warmup != 0 {
		t.Errorf("warmup = %v, want explicit zero", cfg.Load.Warmup)
	}
}

// Misconfiguration that silently runs with defaults is data corruption for a
// measurement tool: every bad config must fail fast, naming the bad key.
func TestConfigRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "probe:\n  alowed_ratio: 0.5\n" // typo'd allowed_ratio
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(path); err == nil {
		t.Fatal("config with unknown key alowed_ratio loaded without error")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "unknown field `alowed_ratio`") ||
			!strings.Contains(msg, "line 2") ||
			!strings.Contains(msg, "did you mean `allowed_ratio`") {
			t.Fatalf("unknown-key error was not actionable: %v", err)
		}
	}

	path = filepath.Join(t.TempDir(), "target.yaml")
	yaml = "probe:\n  targets:\n    - relation: document#viewer\n      weigth: 8\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(path); err == nil {
		t.Fatal("target with unknown key weigth loaded without error")
	} else if !strings.Contains(err.Error(), "weigth") {
		t.Fatalf("target unknown-key error did not name typo: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		"bad consistency":              "load:\n  consistency: EVENTUAL\n",
		"bad endpoint":                 "load:\n  endpoint: expand\n",
		"bad probability":              "seed:\n  wildcard_probability: 1.5\n",
		"bad allowed_ratio":            "probe:\n  allowed_ratio: 2\n",
		"bad fanout key":               "seed:\n  fanout:\n    notarelation: 3\n",
		"bad target key":               "probe:\n  targets: [document]\n",
		"zero target weight":           "probe:\n  targets:\n    - relation: document#viewer\n      weight: 0\n",
		"bad contextual key":           "contextual:\n  relations: [viewer]\n",
		"missing pool":                 "conditions:\n  has_scope:\n    params:\n      scopes: {pool: nope}\n",
		"negative rate":                "load:\n  rate: -5\n",
		"negative client id":           "load:\n  client_id: -1\n",
		"negative concurrency":         "load:\n  concurrency: -1\n",
		"zero concurrency":             "load:\n  concurrency: 0\n",
		"negative openfga timeout":     "openfga:\n  timeout: -1s\n",
		"negative warmup":              "load:\n  warmup: -1s\n",
		"zero duration":                "load:\n  duration: 0s\n",
		"negative duration":            "load:\n  duration: -1s\n",
		"negative sweep duration":      "load:\n  sweep:\n    rates: [100]\n    step_duration: -1s\n",
		"zero sweep duration":          "load:\n  sweep:\n    rates: [100]\n    step_duration: 0s\n",
		"negative default fanout":      "seed:\n  default_fanout: -1\n",
		"negative fanout":              "seed:\n  fanout:\n    document#viewer: -1\n",
		"negative instances":           "seed:\n  instances:\n    document: -1\n",
		"negative pool count":          "pools:\n  default:\n    prefix: val-\n    count: -1\n",
		"negative condition keys":      "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys: -1}\n",
		"empty fanout user type":       "seed:\n  fanout:\n    \"group#member@\": 3\n",
		"bad wildcard prob key":        "seed:\n  wildcard_probabilities:\n    notarelation: 0.5\n",
		"wildcard prob out of range":   "seed:\n  wildcard_probabilities:\n    \"document#viewer\": 1.5\n",
		"keys and keys_distribution":   "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys: 4, keys_distribution: {values: [2, 8]}}\n",
		"empty distribution values":    "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys_distribution: {values: []}}\n",
		"mismatched weights":           "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys_distribution: {values: [2, 8], weights: [1]}}\n",
		"nonpositive value":            "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys_distribution: {values: [0]}}\n",
		"zero-sum weights":             "conditions:\n  has_scope:\n    params:\n      granted_scopes: {keys_distribution: {values: [2, 8], weights: [0, 0]}}\n",
		"attribute_ds without metrics": "probe:\n  attribute_ds_queries: true\n",
	}
	for name, yaml := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(path); err == nil {
			t.Errorf("%s: config loaded without error:\n%s", name, yaml)
		}
	}
}

func TestValidateAgainstModel(t *testing.T) {
	a := loadExampleModel(t)

	good, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	good.Seed.Fanout = map[string]int{"document#viewer": 3, "document#viewer@user": 1, "document#viewer@group#member": 4}
	good.Seed.WildcardProbs = map[string]float64{"document#viewer": 0.2}
	good.Seed.Instances = map[string]int{"document": 10}
	good.Probe.Targets = []TargetSpec{{Relation: "document#can_share", Weight: 1}}
	good.Probe.SubjectTypes = []string{"user"}
	good.Contextual.Relations = []string{"document#active_context"}
	good.Conditions = map[string]CondConfig{"has_scope": {}}
	if err := good.validateAgainstModel(a); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	bad := []func(c *Config){
		func(c *Config) { c.Seed.Fanout = map[string]int{"document#vewer": 3} },
		func(c *Config) { c.Seed.Instances = map[string]int{"documnet": 10} },
		func(c *Config) { c.Probe.Targets = []TargetSpec{{Relation: "document#nope", Weight: 1}} },
		func(c *Config) { c.Probe.SubjectTypes = []string{"robot"} },
		func(c *Config) { c.Contextual.Relations = []string{"folder#active_context"} },
		func(c *Config) { c.Contextual.Relations = []string{"document#can_view"} },
		func(c *Config) { c.Conditions = map[string]CondConfig{"no_such_condition": {}} },
		// suffix names a user type the relation does not directly accept
		func(c *Config) { c.Seed.Fanout = map[string]int{"document#can_share@group#member": 3} },
		// relation exists but has no wildcard ref
		func(c *Config) { c.Seed.WildcardProbs = map[string]float64{"document#editor": 0.5} },
		func(c *Config) { c.Seed.WildcardProbs = map[string]float64{"document#nope": 0.5} },
		func(c *Config) { c.Conditions = map[string]CondConfig{"has_scope": {TupleParams: []string{"nope"}}} },
		func(c *Config) {
			c.Conditions = map[string]CondConfig{"has_scope": {ParamConfigs: map[string]ParamGenConfig{"nope": {Keys: 1}}}}
		},
	}
	for i, mutate := range bad {
		cfg, _ := LoadConfigFile("")
		mutate(cfg)
		if err := cfg.validateAgainstModel(a); err == nil {
			t.Errorf("bad config %d passed model validation", i)
		}
	}
}

// OIDC config: incomplete or conflicting auth must fail; a valid block must
// redact the client secret in the resolved snapshot.
func TestOIDCConfig(t *testing.T) {
	bad := map[string]string{
		"oidc missing fields": "openfga:\n  oidc:\n    token_url: https://issuer/token\n",
		"oidc and api_token":  "openfga:\n  api_token: psk\n  oidc:\n    token_url: https://issuer/token\n    client_id: id\n    client_secret: sec\n",
	}
	for name, yaml := range bad {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(path); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "openfga:\n  oidc:\n    token_url: https://issuer/token\n    client_id: id\n    client_secret: super-secret\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("valid OIDC config rejected: %v", err)
	}
	resolved := cfg.Resolved()
	of := resolved["openfga"].(map[string]any)
	oidc := of["oidc"].(map[string]any)
	if oidc["client_secret"] != "REDACTED" {
		t.Errorf("client_secret not redacted: %v", oidc["client_secret"])
	}
}

// list-objects and list-users are valid load endpoints.
func TestListEndpointsValid(t *testing.T) {
	for _, ep := range []string{"check", "batch-check", "list-objects", "list-users"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("load:\n  endpoint: "+ep+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(path); err != nil {
			t.Errorf("endpoint %q rejected: %v", ep, err)
		}
	}
}

// probe.attribute_ds_queries is valid (and parses) when a metrics endpoint is
// configured; off-by-default keeps the normal probe path untouched.
func TestAttributeDSConfig(t *testing.T) {
	def, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	if def.Probe.AttributeDS {
		t.Error("probe.attribute_ds_queries should default to false")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "metrics:\n  prometheus_url: http://localhost:2112\nprobe:\n  attribute_ds_queries: true\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("attribute_ds_queries with metrics endpoint rejected: %v", err)
	}
	if !cfg.Probe.AttributeDS {
		t.Error("attribute_ds_queries: true did not parse")
	}
}

// probe.targets accepts both bare strings and {relation, weight} maps.
func TestTargetSpecYAMLForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "probe:\n  targets:\n    - document#viewer\n    - relation: document#editor\n      weight: 8\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []TargetSpec{{Relation: "document#viewer", Weight: 1}, {Relation: "document#editor", Weight: 8}}
	if len(cfg.Probe.Targets) != 2 || cfg.Probe.Targets[0] != want[0] || cfg.Probe.Targets[1] != want[1] {
		t.Errorf("targets = %+v, want %+v", cfg.Probe.Targets, want)
	}
}

// CLI flag overrides replace the matching config value, leave everything else
// alone, and re-validate (so a bad override fails fast).
func TestApplyOverrides(t *testing.T) {
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	dur := 5 * time.Second
	rate := 1234
	clientID := 3
	endpoint := "batch-check"
	out := "/tmp/elsewhere"
	if err := cfg.applyOverrides(Overrides{Duration: &dur, Rate: &rate, ClientID: &clientID, Endpoint: &endpoint, OutputDir: &out}); err != nil {
		t.Fatal(err)
	}
	if cfg.Load.Duration != dur || cfg.Load.Rate != rate || cfg.Load.ClientID != clientID || cfg.Load.Endpoint != endpoint || cfg.OutputDir != out {
		t.Errorf("overrides not applied: %+v", cfg.Load)
	}
	if cfg.Load.Warmup == 0 || cfg.Load.Consistency == "" {
		t.Errorf("untouched fields lost their defaults: warmup=%v consistency=%q", cfg.Load.Warmup, cfg.Load.Consistency)
	}

	// An empty Overrides is a no-op.
	beforeRate, beforeDur := cfg.Load.Rate, cfg.Load.Duration
	if err := cfg.applyOverrides(Overrides{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Load.Rate != beforeRate || cfg.Load.Duration != beforeDur {
		t.Errorf("empty overrides changed config: rate %d->%d dur %v->%v", beforeRate, cfg.Load.Rate, beforeDur, cfg.Load.Duration)
	}

	// A bad override must fail validation.
	bad := "EVENTUAL"
	if err := cfg.applyOverrides(Overrides{Consistency: &bad}); err == nil {
		t.Error("invalid consistency override accepted")
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
