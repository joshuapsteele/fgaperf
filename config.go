package main

// config.go defines the YAML configuration. Every field has a usable default;
// an empty config file against any model should produce a working run.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	OpenFGA      OpenFGAConfig         `yaml:"openfga"`
	ModelFile    string                `yaml:"model_file"`
	StateFile    string                `yaml:"state_file"`
	CorpusFile   string                `yaml:"corpus_file"`
	CorpusSource string                `yaml:"corpus_source"` // how `probe` builds the corpus: probe (synthesize from the model) | replay (a real check log)
	OutputDir    string                `yaml:"output_dir"`
	Seed         SeedConfig            `yaml:"seed"`
	Contextual   ContextualConfig      `yaml:"contextual"`
	Probe        ProbeConfig           `yaml:"probe"`
	Replay       ReplayConfig          `yaml:"replay"`
	Load         LoadConfig            `yaml:"load"`
	Metrics      MetricsConfig         `yaml:"metrics"`
	Conditions   map[string]CondConfig `yaml:"conditions"`
	Pools        map[string]PoolConfig `yaml:"pools"`
	RandomSeed   int64                 `yaml:"random_seed"`
	KeepStore    bool                  `yaml:"keep_store"` // keep the store after `all` instead of deleting it
}

type OpenFGAConfig struct {
	APIURL    string        `yaml:"api_url"`
	GRPCURL   string        `yaml:"grpc_url"` // host:port for the gRPC API (load.transport: grpc); empty = derive host:8081 from api_url
	GRPCTLS   bool          `yaml:"grpc_tls"` // dial the gRPC endpoint over TLS (managed/cloud OpenFGA); false = plaintext (local)
	StoreName string        `yaml:"store_name"`
	APIToken  string        `yaml:"api_token"` // pre-shared key auth
	OIDC      *OIDCConfig   `yaml:"oidc"`      // OIDC client-credentials auth (mutually exclusive with api_token)
	Timeout   time.Duration `yaml:"timeout"`
}

// OIDCConfig configures OAuth2 client-credentials auth for managed/cloud
// OpenFGA. The token is fetched and refreshed in the background, off the
// request hot path.
type OIDCConfig struct {
	TokenURL     string   `yaml:"token_url"` // OAuth2 token endpoint
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Audience     string   `yaml:"audience"` // optional
	Scopes       []string `yaml:"scopes"`   // optional
}

type SeedConfig struct {
	Cohorts       int            `yaml:"cohorts"`
	DefaultCount  int            `yaml:"default_instances"`
	Instances     map[string]int `yaml:"instances"`      // per-type instance counts
	DefaultFanout int            `yaml:"default_fanout"` // tuples per (object, relation, user type)
	// Fanout overrides are keyed "type#relation" (every accepted user type)
	// or "type#relation@usertype" (just that user type; usersets are named
	// "type#relation@group#member"). The bare key is the default for user
	// types without a suffixed override.
	Fanout        map[string]int     `yaml:"fanout"`
	BatchSize     int                `yaml:"batch_size"` // tuples per Write call (server default max: 100)
	Writers       int                `yaml:"writers"`    // concurrent Write workers
	WildcardProb  float64            `yaml:"wildcard_probability"`
	WildcardProbs map[string]float64 `yaml:"wildcard_probabilities"` // per-relation overrides keyed "type#relation"
}

type ContextualConfig struct {
	Relations         []string `yaml:"relations"`          // direct "type#relation" tuples supplied per check instead of seeded
	AttachProbability *float64 `yaml:"attach_probability"` // probability each sampled check carries its contextual tuples; default 1 when relations are set
}

type ProbeConfig struct {
	Targets        []TargetSpec `yaml:"targets"`       // empty = all relations, weight 1
	SubjectTypes   []string     `yaml:"subject_types"` // empty = inferred terminal types
	Samples        int          `yaml:"samples_per_target"`
	CohortBias     float64      `yaml:"cohort_bias"`
	AllowedRatio   float64      `yaml:"allowed_ratio"`   // -1 keeps the natural mix
	MaxDuplication float64      `yaml:"max_duplication"` // resample duplication bound; -1 = unbounded
	Concurrency    int          `yaml:"concurrency"`
	// AttributeDS turns on a best-effort per-relation datastore-query
	// attribution pass after the corpus is built: it replays a small distinct
	// batch per target, one relation at a time, and diffs OpenFGA's
	// openfga_datastore_query_count histogram to estimate datastore queries per
	// check. Requires metrics.prometheus_url. Off by default; when off the
	// probe path is byte-identical.
	AttributeDS bool `yaml:"attribute_ds_queries"`
}

// TargetSpec names a probe target. In YAML it is either a bare string
// ("document#viewer", weight 1) or a map with an optional traffic weight
// ({relation: document#viewer, weight: 8}). Weights skew the load phase's
// traffic mix toward production-like shares; probing itself samples every
// target equally.
type TargetSpec struct {
	Relation string  `yaml:"relation"` // "type#relation"
	Weight   float64 `yaml:"weight"`
}

func (t *TargetSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		t.Relation = node.Value
		t.Weight = 1
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("probe target must be a relation string or mapping")
	}
	var weightSet bool
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "relation":
			if err := val.Decode(&t.Relation); err != nil {
				return err
			}
		case "weight":
			weightSet = true
			if err := val.Decode(&t.Weight); err != nil {
				return err
			}
		default:
			return fmt.Errorf("field %s not found in type main.TargetSpec", key.Value)
		}
	}
	if !weightSet {
		t.Weight = 1
	}
	return nil
}

// ReplayConfig points the corpus builder at a real check log when
// corpus_source is "replay". The file is a JSONL of OpenFGA check requests —
// one {user, relation, object[, contextual_tuples, context]} object per line —
// whose natural per-target frequencies become the load-phase traffic mix.
type ReplayConfig struct {
	File string `yaml:"file"` // JSONL check log to replay; required when corpus_source: replay
}

type LoadConfig struct {
	Concurrency   int           `yaml:"concurrency"`
	ClientID      int           `yaml:"client_id"` // distinct RNG stream for multi-client runs; 0 = default single client
	Transport     string        `yaml:"transport"` // wire protocol for the measured phase: http (default) | grpc. Setup/probe always use HTTP.
	Rate          int           `yaml:"rate"`      // requests/sec; 0 = closed loop
	Arrival       string        `yaml:"arrival"`   // fixed-rate arrival process: uniform (even ticker) | poisson (exponential inter-arrivals)
	Warmup        time.Duration `yaml:"warmup"`
	Duration      time.Duration `yaml:"duration"`
	Consistency   string        `yaml:"consistency"`    // MINIMIZE_LATENCY | HIGHER_CONSISTENCY
	Endpoint      string        `yaml:"endpoint"`       // check | batch-check | list-objects | list-users
	BatchSize     int           `yaml:"batch_size"`     // for batch-check
	VerifyResults bool          `yaml:"verify_results"` // compare allowed against probe expectations
	WriteRate     int           `yaml:"write_rate"`     // background tuple writes/sec during the measured phase; 0 = none
	SampleFile    string        `yaml:"sample_file"`    // optional: dump one JSON line per measured sample here (.gz = gzip); "" = off
	Sweep         SweepConfig   `yaml:"sweep"`
	SLOP99        time.Duration `yaml:"slo_p99"` // optional: a sweep step "passes" only when response-latency p99 is under this

	// sampleAppend makes RunLoad open SampleFile in append mode instead of
	// truncating, so a sweep's steps all land in one file. Not a YAML knob;
	// set by RunSweep per step.
	sampleAppend bool `yaml:"-"`
}

// SweepConfig steps through fixed offered rates in one run, reusing the same
// corpus and seeded store, to locate the saturation knee.
type SweepConfig struct {
	Rates        []int         `yaml:"rates"`         // offered req/s per step; empty = no sweep
	StepDuration time.Duration `yaml:"step_duration"` // measured duration per step
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
	TupleParams    []string                  `yaml:"tuple_params"` // params bound at write time
	ParamConfigs   map[string]ParamGenConfig `yaml:"params"`
	tupleParamsSet bool                      `yaml:"-"`
}

func (c *CondConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("condition config must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "tuple_params":
			c.tupleParamsSet = true
		case "params":
		default:
			return fmt.Errorf("field %s not found in type main.CondConfig", node.Content[i].Value)
		}
	}
	type plain CondConfig
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	p.tupleParamsSet = c.tupleParamsSet
	*c = CondConfig(p)
	return nil
}

type ParamGenConfig struct {
	Pool string `yaml:"pool"`
	Keys int    `yaml:"keys"` // fixed entry count for map/list params
	// KeysDistribution draws the entry count per tuple instead, so one run
	// can mix mostly-small and occasionally-huge maps the way real datasets
	// skew. Mutually exclusive with keys.
	KeysDistribution *KeysDistribution `yaml:"keys_distribution"`
}

// KeysDistribution is a weighted discrete distribution over map/list sizes.
// Empty weights means uniform over values.
type KeysDistribution struct {
	Values  []int     `yaml:"values"`
	Weights []float64 `yaml:"weights"`
}

func (d *KeysDistribution) draw(rng *rand.Rand) int {
	if len(d.Weights) == 0 {
		return d.Values[rng.Intn(len(d.Values))]
	}
	total := 0.0
	for _, w := range d.Weights {
		total += w
	}
	x := rng.Float64() * total
	for i, w := range d.Weights {
		x -= w
		if x < 0 {
			return d.Values[i]
		}
	}
	return d.Values[len(d.Values)-1]
}

type PoolConfig struct {
	Values []string `yaml:"values"`
	Prefix string   `yaml:"prefix"`
	Count  int      `yaml:"count"`
}

func LoadConfigFile(path string) (*Config, error) {
	cfg := &Config{}
	var fields yamlFieldSet
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		fields, err = parseYAMLFieldSet(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
		// Unknown keys are fatal: a typo'd knob silently running with defaults
		// produces numbers that look configured but aren't.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, configDecodeError(path, raw, err)
		}
	}
	cfg.applyDefaults(fields)
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

type yamlFieldSet map[string]bool

func parseYAMLFieldSet(raw []byte) (yamlFieldSet, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	fields := yamlFieldSet{}
	if len(doc.Content) > 0 {
		collectYAMLFields("", doc.Content[0], fields)
	}
	return fields, nil
}

func collectYAMLFields(prefix string, node *yaml.Node, fields yamlFieldSet) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		fields[path] = true
		collectYAMLFields(path, node.Content[i+1], fields)
	}
}

func (f yamlFieldSet) has(parts ...string) bool {
	if f == nil {
		return false
	}
	return f[strings.Join(parts, ".")]
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
	case "check", "batch-check", "list-objects", "list-users":
	default:
		return fmt.Errorf("load.endpoint must be check, batch-check, list-objects, or list-users, got %q", c.Load.Endpoint)
	}
	switch c.Load.Arrival {
	case "uniform", "poisson":
	default:
		return fmt.Errorf("load.arrival must be uniform or poisson, got %q", c.Load.Arrival)
	}
	switch c.Load.Transport {
	case "http", "grpc":
	default:
		return fmt.Errorf("load.transport must be http or grpc, got %q", c.Load.Transport)
	}
	switch c.CorpusSource {
	case "probe", "replay":
	default:
		return fmt.Errorf("corpus_source must be probe or replay, got %q", c.CorpusSource)
	}
	if c.CorpusSource == "replay" && c.Replay.File == "" {
		return fmt.Errorf("corpus_source: replay requires replay.file (a JSONL check log to replay)")
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
	if c.Probe.AttributeDS && c.Metrics.PrometheusURL == "" {
		return fmt.Errorf("probe.attribute_ds_queries requires metrics.prometheus_url (the per-relation datastore-query attribution pass reads OpenFGA's metrics endpoint)")
	}
	if c.Contextual.AttachProbability != nil {
		if err := prob("contextual.attach_probability", *c.Contextual.AttachProbability); err != nil {
			return err
		}
	}
	for k := range c.Seed.Fanout {
		rel, userType := splitFanoutKey(k)
		if !isTypeRelation(rel) || (strings.Contains(k, "@") && userType == "") {
			return fmt.Errorf("seed.fanout key %q must be of the form type#relation or type#relation@usertype", k)
		}
	}
	for k, v := range c.Seed.WildcardProbs {
		if !isTypeRelation(k) {
			return fmt.Errorf("seed.wildcard_probabilities key %q must be of the form type#relation", k)
		}
		if err := prob("seed.wildcard_probabilities."+k, v); err != nil {
			return err
		}
	}
	for _, k := range c.Contextual.Relations {
		if !isTypeRelation(k) {
			return fmt.Errorf("contextual.relations key %q must be of the form type#relation", k)
		}
	}
	for _, t := range c.Probe.Targets {
		if !isTypeRelation(t.Relation) {
			return fmt.Errorf("probe.targets entry %q must be of the form type#relation", t.Relation)
		}
		if t.Weight <= 0 {
			return fmt.Errorf("probe.targets weight for %q must be positive, got %v", t.Relation, t.Weight)
		}
	}
	for cond, cc := range c.Conditions {
		for param, pc := range cc.ParamConfigs {
			if pc.Pool != "" {
				if _, ok := c.Pools[pc.Pool]; !ok {
					return fmt.Errorf("conditions.%s.params.%s references pool %q, which is not defined under pools", cond, param, pc.Pool)
				}
			}
			if pc.Keys < 0 {
				return fmt.Errorf("conditions.%s.params.%s.keys must be >= 0, got %d", cond, param, pc.Keys)
			}
			if d := pc.KeysDistribution; d != nil {
				name := fmt.Sprintf("conditions.%s.params.%s.keys_distribution", cond, param)
				if pc.Keys > 0 {
					return fmt.Errorf("conditions.%s.params.%s sets both keys and keys_distribution; pick one", cond, param)
				}
				if len(d.Values) == 0 {
					return fmt.Errorf("%s.values must not be empty", name)
				}
				if len(d.Weights) > 0 && len(d.Weights) != len(d.Values) {
					return fmt.Errorf("%s has %d weights for %d values", name, len(d.Weights), len(d.Values))
				}
				total := 0.0
				for i, v := range d.Values {
					if v <= 0 {
						return fmt.Errorf("%s.values must all be positive, got %d", name, v)
					}
					if len(d.Weights) > 0 {
						if d.Weights[i] < 0 {
							return fmt.Errorf("%s.weights must all be >= 0, got %v", name, d.Weights[i])
						}
						total += d.Weights[i]
					}
				}
				if len(d.Weights) > 0 && total <= 0 {
					return fmt.Errorf("%s.weights must sum to a positive value", name)
				}
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
	nonnegative := func(name string, v int) error {
		if v < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", name, v)
		}
		return nil
	}
	if err := nonnegative("seed.default_fanout", c.Seed.DefaultFanout); err != nil {
		return err
	}
	for typ, n := range c.Seed.Instances {
		if err := nonnegative("seed.instances."+typ, n); err != nil {
			return err
		}
	}
	for key, n := range c.Seed.Fanout {
		if err := nonnegative("seed.fanout."+key, n); err != nil {
			return err
		}
	}
	for name, p := range c.Pools {
		if err := nonnegative("pools."+name+".count", p.Count); err != nil {
			return err
		}
	}
	if c.Load.Rate < 0 {
		return fmt.Errorf("load.rate must be >= 0 (0 = closed loop), got %d", c.Load.Rate)
	}
	if c.Load.WriteRate < 0 {
		return fmt.Errorf("load.write_rate must be >= 0, got %d", c.Load.WriteRate)
	}
	if c.Load.ClientID < 0 {
		return fmt.Errorf("load.client_id must be >= 0, got %d", c.Load.ClientID)
	}
	if c.OpenFGA.Timeout < 0 {
		return fmt.Errorf("openfga.timeout must be >= 0, got %v", c.OpenFGA.Timeout)
	}
	if c.Load.Warmup < 0 {
		return fmt.Errorf("load.warmup must be >= 0, got %v", c.Load.Warmup)
	}
	if c.Load.Duration <= 0 {
		return fmt.Errorf("load.duration must be positive, got %v", c.Load.Duration)
	}
	if c.Load.Sweep.StepDuration < 0 {
		return fmt.Errorf("load.sweep.step_duration must be >= 0, got %v", c.Load.Sweep.StepDuration)
	}
	for _, r := range c.Load.Sweep.Rates {
		if r <= 0 {
			return fmt.Errorf("load.sweep.rates must all be positive, got %d", r)
		}
	}
	if len(c.Load.Sweep.Rates) > 0 && c.Load.Rate > 0 {
		return fmt.Errorf("load.rate and load.sweep are mutually exclusive; sweep sets the rate per step")
	}
	if len(c.Load.Sweep.Rates) > 0 && c.Load.Sweep.StepDuration <= 0 {
		return fmt.Errorf("load.sweep.step_duration must be positive when load.sweep.rates is set, got %v", c.Load.Sweep.StepDuration)
	}
	if c.Load.SLOP99 < 0 {
		return fmt.Errorf("load.slo_p99 must be >= 0, got %v", c.Load.SLOP99)
	}
	if o := c.OpenFGA.OIDC; o != nil {
		if c.OpenFGA.APIToken != "" {
			return fmt.Errorf("openfga.api_token and openfga.oidc are mutually exclusive; pick one auth method")
		}
		if o.TokenURL == "" || o.ClientID == "" || o.ClientSecret == "" {
			return fmt.Errorf("openfga.oidc requires token_url, client_id, and client_secret")
		}
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
	for k := range c.Seed.Fanout {
		rel, userType := splitFanoutKey(k)
		if !relations[rel] {
			return fmt.Errorf("seed.fanout names relation %q, which is not in the model", rel)
		}
		if userType == "" {
			continue
		}
		typ, relName, _ := strings.Cut(rel, "#")
		accepted := false
		for _, ref := range a.DirectRefs[typ][relName] {
			name := ref.Type
			if ref.Relation != "" {
				name += "#" + ref.Relation
			}
			if name == userType {
				accepted = true
			}
		}
		if !accepted {
			return fmt.Errorf("seed.fanout key %q: relation %s does not directly accept user type %q", k, rel, userType)
		}
	}
	for k := range c.Seed.WildcardProbs {
		if !relations[k] {
			return fmt.Errorf("seed.wildcard_probabilities names relation %q, which is not in the model", k)
		}
		typ, relName, _ := strings.Cut(k, "#")
		hasWildcard := false
		for _, ref := range a.DirectRefs[typ][relName] {
			if ref.Wildcard != nil {
				hasWildcard = true
			}
		}
		if !hasWildcard {
			return fmt.Errorf("seed.wildcard_probabilities names relation %q, which does not accept a wildcard", k)
		}
	}
	for _, k := range c.Contextual.Relations {
		if !relations[k] {
			return fmt.Errorf("contextual.relations names relation %q, which is not in the model", k)
		}
		typ, relName, _ := strings.Cut(k, "#")
		refs := a.DirectRefs[typ][relName]
		if len(refs) == 0 {
			return fmt.Errorf("contextual.relations names relation %q, which does not accept direct tuples; use the direct relation that carries the request-local fact, not the computed relation that depends on it", k)
		}
		if !hasPlainDirectRef(refs) {
			return fmt.Errorf("contextual.relations names relation %q, which does not accept plain direct user tuples", k)
		}
	}
	for _, t := range c.Probe.Targets {
		if !relations[t.Relation] {
			return fmt.Errorf("probe.targets names relation %q, which is not in the model", t.Relation)
		}
	}
	for cond := range c.Conditions {
		modelCond, ok := a.Model.Conditions[cond]
		if !ok {
			return fmt.Errorf("conditions names condition %q, which is not in the model", cond)
		}
		knownParams := map[string]bool{}
		for param := range modelCond.Parameters {
			knownParams[param] = true
		}
		for _, param := range c.Conditions[cond].TupleParams {
			if !knownParams[param] {
				return fmt.Errorf("conditions.%s.tuple_params names parameter %q, which is not declared by the condition", cond, param)
			}
		}
		for param := range c.Conditions[cond].ParamConfigs {
			if !knownParams[param] {
				return fmt.Errorf("conditions.%s.params names parameter %q, which is not declared by the condition", cond, param)
			}
		}
	}
	for _, st := range c.Probe.SubjectTypes {
		if !types[st] {
			return fmt.Errorf("probe.subject_types names type %q, which is not in the model", st)
		}
	}
	return nil
}

// splitFanoutKey splits "type#relation@usertype" into the relation key and
// the optional user-type suffix ("" when absent). Userset suffixes keep
// their own #: "document#viewer@group#member" -> ("document#viewer",
// "group#member").
func splitFanoutKey(key string) (rel, userType string) {
	if i := strings.Index(key, "@"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

func isTypeRelation(key string) bool {
	parts := strings.SplitN(key, "#", 2)
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func (c *Config) applyDefaults(fields yamlFieldSet) {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	defInt := func(i *int, v int, path ...string) {
		if *i == 0 && !fields.has(path...) {
			*i = v
		}
	}
	def(&c.OpenFGA.APIURL, "http://localhost:8080")
	def(&c.OpenFGA.StoreName, "fgaperf")
	if c.OpenFGA.Timeout == 0 && !fields.has("openfga", "timeout") {
		c.OpenFGA.Timeout = 10 * time.Second
	}
	def(&c.ModelFile, "model.json")
	def(&c.StateFile, ".fgaperf-state.json")
	def(&c.CorpusFile, "corpus.json")
	def(&c.CorpusSource, "probe")
	def(&c.OutputDir, "results")

	defInt(&c.Seed.Cohorts, 5, "seed", "cohorts")
	defInt(&c.Seed.DefaultCount, 25, "seed", "default_instances")
	defInt(&c.Seed.DefaultFanout, 2, "seed", "default_fanout")
	defInt(&c.Seed.BatchSize, 100, "seed", "batch_size")
	defInt(&c.Seed.Writers, 8, "seed", "writers")
	if c.Seed.WildcardProb == 0 && !fields.has("seed", "wildcard_probability") {
		c.Seed.WildcardProb = 1.0
	}

	defInt(&c.Probe.Samples, 200, "probe", "samples_per_target")
	defInt(&c.Probe.Concurrency, 8, "probe", "concurrency")
	if c.Probe.CohortBias == 0 && !fields.has("probe", "cohort_bias") {
		c.Probe.CohortBias = 0.85
	}
	if c.Probe.AllowedRatio == 0 && !fields.has("probe", "allowed_ratio") {
		c.Probe.AllowedRatio = 0.5
	}
	if c.Probe.MaxDuplication == 0 && !fields.has("probe", "max_duplication") {
		c.Probe.MaxDuplication = 5
	}

	defInt(&c.Load.Concurrency, 16, "load", "concurrency")
	if c.Load.Warmup == 0 && !fields.has("load", "warmup") {
		c.Load.Warmup = 10 * time.Second
	}
	if c.Load.Duration == 0 && !fields.has("load", "duration") {
		c.Load.Duration = 60 * time.Second
	}
	def(&c.Load.Consistency, "MINIMIZE_LATENCY")
	def(&c.Load.Endpoint, "check")
	def(&c.Load.Arrival, "uniform")
	def(&c.Load.Transport, "http")
	defInt(&c.Load.BatchSize, 20, "load", "batch_size")
	if len(c.Load.Sweep.Rates) > 0 && c.Load.Sweep.StepDuration == 0 && !fields.has("load", "sweep", "step_duration") {
		c.Load.Sweep.StepDuration = 60 * time.Second
	}

	if c.Pools == nil {
		c.Pools = map[string]PoolConfig{}
	}
	if _, ok := c.Pools["default"]; !ok {
		c.Pools["default"] = PoolConfig{Prefix: "val-", Count: 16}
	}
}

// Overrides carries CLI flag values that override config after load. A nil
// field means the flag was not passed; only explicitly set flags are applied,
// so a missing flag never clobbers a configured value with a zero.
type Overrides struct {
	Duration    *time.Duration
	Warmup      *time.Duration
	Rate        *int
	Concurrency *int
	ClientID    *int
	Endpoint    *string
	Consistency *string
	Transport   *string
	OutputDir   *string
}

// applyOverrides applies CLI flag overrides on top of the loaded config and
// re-validates. Overrides mutate cfg before Resolved() is marshaled, so they
// are recorded in the results snapshot like any other knob.
func (c *Config) applyOverrides(o Overrides) error {
	if o.Duration != nil {
		c.Load.Duration = *o.Duration
	}
	if o.Warmup != nil {
		c.Load.Warmup = *o.Warmup
	}
	if o.Rate != nil {
		c.Load.Rate = *o.Rate
	}
	if o.Concurrency != nil {
		c.Load.Concurrency = *o.Concurrency
	}
	if o.ClientID != nil {
		c.Load.ClientID = *o.ClientID
	}
	if o.Endpoint != nil {
		c.Load.Endpoint = *o.Endpoint
	}
	if o.Consistency != nil {
		c.Load.Consistency = *o.Consistency
	}
	if o.Transport != nil {
		c.Load.Transport = *o.Transport
	}
	if o.OutputDir != nil {
		c.OutputDir = *o.OutputDir
	}
	return c.validate()
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
		if oidc, ok := of["oidc"].(map[string]any); ok {
			if sec, ok := oidc["client_secret"].(string); ok && sec != "" {
				oidc["client_secret"] = "REDACTED"
			}
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
