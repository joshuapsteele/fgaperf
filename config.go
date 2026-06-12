package main

// config.go defines the YAML configuration. Every field has a usable default;
// an empty config file against any model should produce a working run.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	OpenFGA    OpenFGAConfig         `yaml:"openfga"`
	ModelFile  string                `yaml:"model_file"`
	StateFile  string                `yaml:"state_file"`
	CorpusFile string                `yaml:"corpus_file"`
	OutputDir  string                `yaml:"output_dir"`
	Seed       SeedConfig            `yaml:"seed"`
	Contextual ContextualConfig      `yaml:"contextual"`
	Probe      ProbeConfig           `yaml:"probe"`
	Load       LoadConfig            `yaml:"load"`
	Metrics    MetricsConfig         `yaml:"metrics"`
	Conditions map[string]CondConfig `yaml:"conditions"`
	Pools      map[string]PoolConfig `yaml:"pools"`
	RandomSeed int64                 `yaml:"random_seed"`
	KeepStore  bool                  `yaml:"keep_store"` // keep the store after `all` instead of deleting it
}

type OpenFGAConfig struct {
	APIURL    string        `yaml:"api_url"`
	StoreName string        `yaml:"store_name"`
	APIToken  string        `yaml:"api_token"`
	Timeout   time.Duration `yaml:"timeout"`
}

type SeedConfig struct {
	Cohorts       int            `yaml:"cohorts"`
	DefaultCount  int            `yaml:"default_instances"`
	Instances     map[string]int `yaml:"instances"`      // per-type instance counts
	DefaultFanout int            `yaml:"default_fanout"` // tuples per (object, relation, user type)
	Fanout        map[string]int `yaml:"fanout"`         // overrides keyed "type#relation"
	BatchSize     int            `yaml:"batch_size"`     // tuples per Write call (server default max: 100)
	Writers       int            `yaml:"writers"`        // concurrent Write workers
	WildcardProb  float64        `yaml:"wildcard_probability"`
}

type ContextualConfig struct {
	Relations         []string `yaml:"relations"`          // direct "type#relation" tuples supplied per check instead of seeded
	AttachProbability *float64 `yaml:"attach_probability"` // probability each sampled check carries its contextual tuples; default 1 when relations are set
}

type ProbeConfig struct {
	Targets        []string `yaml:"targets"`       // "type#relation"; empty = all relations
	SubjectTypes   []string `yaml:"subject_types"` // empty = inferred terminal types
	Samples        int      `yaml:"samples_per_target"`
	CohortBias     float64  `yaml:"cohort_bias"`
	AllowedRatio   float64  `yaml:"allowed_ratio"`   // -1 keeps the natural mix
	MaxDuplication float64  `yaml:"max_duplication"` // resample duplication bound; -1 = unbounded
	Concurrency    int      `yaml:"concurrency"`
}

type LoadConfig struct {
	Concurrency   int           `yaml:"concurrency"`
	Rate          int           `yaml:"rate"` // requests/sec; 0 = closed loop
	Warmup        time.Duration `yaml:"warmup"`
	Duration      time.Duration `yaml:"duration"`
	Consistency   string        `yaml:"consistency"`    // MINIMIZE_LATENCY | HIGHER_CONSISTENCY
	Endpoint      string        `yaml:"endpoint"`       // check | batch-check
	BatchSize     int           `yaml:"batch_size"`     // for batch-check
	VerifyResults bool          `yaml:"verify_results"` // compare allowed against probe expectations
}

type MetricsConfig struct {
	// PrometheusURL points at OpenFGA's metrics endpoint (the bundled compose
	// stack publishes http://localhost:2112). When set, the run scrapes it at
	// the start and end of the measured phase and reports the server-side
	// view: request duration, datastore queries per check, dispatches, cache
	// hits. Unset or unreachable degrades to the client-side-only report.
	PrometheusURL string `yaml:"prometheus_url"`
}

type CondConfig struct {
	TupleParams  []string                  `yaml:"tuple_params"` // params bound at write time
	ParamConfigs map[string]ParamGenConfig `yaml:"params"`
}

type ParamGenConfig struct {
	Pool string `yaml:"pool"`
	Keys int    `yaml:"keys"` // entries for map/list params
}

type PoolConfig struct {
	Values []string `yaml:"values"`
	Prefix string   `yaml:"prefix"`
	Count  int      `yaml:"count"`
}

func LoadConfigFile(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		// Unknown keys are fatal: a typo'd knob silently running with defaults
		// produces numbers that look configured but aren't.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// validate checks cross-field invariants that don't need the model. Model-
// dependent keys (fanout, instances, targets, ...) are checked separately in
// validateAgainstModel because `run` and `cleanup` never load the model.
func (c *Config) validate() error {
	switch c.Load.Consistency {
	case "MINIMIZE_LATENCY", "HIGHER_CONSISTENCY":
	default:
		return fmt.Errorf("load.consistency must be MINIMIZE_LATENCY or HIGHER_CONSISTENCY, got %q", c.Load.Consistency)
	}
	switch c.Load.Endpoint {
	case "check", "batch-check":
	default:
		return fmt.Errorf("load.endpoint must be check or batch-check, got %q", c.Load.Endpoint)
	}
	prob := func(name string, v float64) error {
		if v < 0 || v > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", name, v)
		}
		return nil
	}
	if err := prob("seed.wildcard_probability", c.Seed.WildcardProb); err != nil {
		return err
	}
	if err := prob("probe.cohort_bias", c.Probe.CohortBias); err != nil {
		return err
	}
	if c.Probe.AllowedRatio > 1 {
		return fmt.Errorf("probe.allowed_ratio must be between 0 and 1 (or negative for the natural mix), got %v", c.Probe.AllowedRatio)
	}
	if c.Contextual.AttachProbability != nil {
		if err := prob("contextual.attach_probability", *c.Contextual.AttachProbability); err != nil {
			return err
		}
	}
	for _, group := range []struct {
		name string
		keys []string
	}{
		{"seed.fanout", mapKeys(c.Seed.Fanout)},
		{"contextual.relations", c.Contextual.Relations},
		{"probe.targets", c.Probe.Targets},
	} {
		for _, k := range group.keys {
			if !isTypeRelation(k) {
				return fmt.Errorf("%s key %q must be of the form type#relation", group.name, k)
			}
		}
	}
	for cond, cc := range c.Conditions {
		for param, pc := range cc.ParamConfigs {
			if pc.Pool == "" {
				continue
			}
			if _, ok := c.Pools[pc.Pool]; !ok {
				return fmt.Errorf("conditions.%s.params.%s references pool %q, which is not defined under pools", cond, param, pc.Pool)
			}
		}
	}
	positive := func(name string, v int) error {
		if v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", name, v)
		}
		return nil
	}
	for _, p := range []struct {
		name string
		v    int
	}{
		{"seed.cohorts", c.Seed.Cohorts},
		{"seed.default_instances", c.Seed.DefaultCount},
		{"seed.batch_size", c.Seed.BatchSize},
		{"seed.writers", c.Seed.Writers},
		{"probe.samples_per_target", c.Probe.Samples},
		{"probe.concurrency", c.Probe.Concurrency},
		{"load.concurrency", c.Load.Concurrency},
		{"load.batch_size", c.Load.BatchSize},
	} {
		if err := positive(p.name, p.v); err != nil {
			return err
		}
	}
	if c.Load.Rate < 0 {
		return fmt.Errorf("load.rate must be >= 0 (0 = closed loop), got %d", c.Load.Rate)
	}
	return nil
}

// validateAgainstModel verifies that every config key naming a model type or
// relation actually exists in the loaded model, so a misspelled key fails fast
// instead of silently using defaults or producing an empty corpus.
func (c *Config) validateAgainstModel(a *Analysis) error {
	relations := map[string]bool{}
	for _, tr := range a.AllRelations {
		relations[tr.Key()] = true
	}
	types := map[string]bool{}
	for _, t := range a.Types {
		types[t] = true
	}
	for t := range c.Seed.Instances {
		if !types[t] {
			return fmt.Errorf("seed.instances names type %q, which is not in the model", t)
		}
	}
	for _, group := range []struct {
		name string
		keys []string
	}{
		{"seed.fanout", mapKeys(c.Seed.Fanout)},
		{"contextual.relations", c.Contextual.Relations},
		{"probe.targets", c.Probe.Targets},
	} {
		for _, k := range group.keys {
			if !relations[k] {
				return fmt.Errorf("%s names relation %q, which is not in the model", group.name, k)
			}
		}
	}
	for cond := range c.Conditions {
		if _, ok := a.Model.Conditions[cond]; !ok {
			return fmt.Errorf("conditions names condition %q, which is not in the model", cond)
		}
	}
	for _, st := range c.Probe.SubjectTypes {
		if !types[st] {
			return fmt.Errorf("probe.subject_types names type %q, which is not in the model", st)
		}
	}
	return nil
}

func mapKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func isTypeRelation(key string) bool {
	parts := strings.SplitN(key, "#", 2)
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func (c *Config) applyDefaults() {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	defInt := func(i *int, v int) {
		if *i == 0 {
			*i = v
		}
	}
	def(&c.OpenFGA.APIURL, "http://localhost:8080")
	def(&c.OpenFGA.StoreName, "fgaperf")
	if c.OpenFGA.Timeout == 0 {
		c.OpenFGA.Timeout = 10 * time.Second
	}
	def(&c.ModelFile, "model.json")
	def(&c.StateFile, ".fgaperf-state.json")
	def(&c.CorpusFile, "corpus.json")
	def(&c.OutputDir, "results")

	defInt(&c.Seed.Cohorts, 5)
	defInt(&c.Seed.DefaultCount, 25)
	defInt(&c.Seed.DefaultFanout, 2)
	defInt(&c.Seed.BatchSize, 100)
	defInt(&c.Seed.Writers, 8)
	if c.Seed.WildcardProb == 0 {
		c.Seed.WildcardProb = 1.0
	}

	defInt(&c.Probe.Samples, 200)
	defInt(&c.Probe.Concurrency, 8)
	if c.Probe.CohortBias == 0 {
		c.Probe.CohortBias = 0.85
	}
	if c.Probe.AllowedRatio == 0 {
		c.Probe.AllowedRatio = 0.5
	}
	if c.Probe.MaxDuplication == 0 {
		c.Probe.MaxDuplication = 5
	}

	defInt(&c.Load.Concurrency, 16)
	if c.Load.Warmup == 0 {
		c.Load.Warmup = 10 * time.Second
	}
	if c.Load.Duration == 0 {
		c.Load.Duration = 60 * time.Second
	}
	def(&c.Load.Consistency, "MINIMIZE_LATENCY")
	def(&c.Load.Endpoint, "check")
	defInt(&c.Load.BatchSize, 20)

	if c.Pools == nil {
		c.Pools = map[string]PoolConfig{}
	}
	if _, ok := c.Pools["default"]; !ok {
		c.Pools["default"] = PoolConfig{Prefix: "val-", Count: 16}
	}
}

// Resolved returns the post-defaults config as a generic map (yaml key names
// preserved) for embedding in results JSON, with credentials redacted.
// Results files outlive memory of how they were produced; this plus the
// random seed is everything needed to recreate a run.
func (c *Config) Resolved() map[string]any {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil
	}
	var m map[string]any
	if yaml.Unmarshal(raw, &m) != nil {
		return nil
	}
	if of, ok := m["openfga"].(map[string]any); ok {
		if tok, ok := of["api_token"].(string); ok && tok != "" {
			of["api_token"] = "REDACTED"
		}
	}
	return m
}

func (p PoolConfig) Materialize() []string {
	if len(p.Values) > 0 {
		return p.Values
	}
	n := p.Count
	if n == 0 {
		n = 16
	}
	prefix := p.Prefix
	if prefix == "" {
		prefix = "val-"
	}
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	return out
}
