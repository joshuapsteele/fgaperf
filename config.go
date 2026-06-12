package main

// config.go defines the YAML configuration. Every field has a usable default;
// an empty config file against any model should produce a working run.

import (
	"fmt"
	"os"
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
	Probe      ProbeConfig           `yaml:"probe"`
	Load       LoadConfig            `yaml:"load"`
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

type ProbeConfig struct {
	Targets      []string `yaml:"targets"`       // "type#relation"; empty = all relations
	SubjectTypes []string `yaml:"subject_types"` // empty = inferred terminal types
	Samples      int      `yaml:"samples_per_target"`
	CohortBias   float64  `yaml:"cohort_bias"`
	AllowedRatio float64  `yaml:"allowed_ratio"` // -1 keeps the natural mix
	Concurrency  int      `yaml:"concurrency"`
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
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}
	cfg.applyDefaults()
	return cfg, nil
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
