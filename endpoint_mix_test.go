package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// load.endpoint accepts both a bare endpoint name (the historical scalar form)
// and a weighted mapping of endpoints (the new blend form).
func TestEndpointMixYAMLForms(t *testing.T) {
	t.Run("scalar", func(t *testing.T) {
		cfg := writeAndLoad(t, "load:\n  endpoint: batch-check\n")
		name, ok := cfg.Load.Endpoint.Single()
		if !ok || name != "batch-check" {
			t.Fatalf("scalar endpoint = %q (single=%v), want batch-check", name, ok)
		}
		if cfg.Load.Endpoint.Label() != "batch-check" {
			t.Errorf("Label = %q, want batch-check", cfg.Load.Endpoint.Label())
		}
		// A scalar must round-trip as a scalar so single-endpoint resolved-config
		// output stays byte-identical.
		if v := cfg.Resolved()["load"].(map[string]any)["endpoint"]; v != "batch-check" {
			t.Errorf("resolved endpoint = %#v, want scalar \"batch-check\"", v)
		}
	})

	t.Run("default is check scalar", func(t *testing.T) {
		cfg, err := LoadConfigFile("")
		if err != nil {
			t.Fatal(err)
		}
		if name, ok := cfg.Load.Endpoint.Single(); !ok || name != "check" {
			t.Fatalf("default endpoint = %q (single=%v), want check", name, ok)
		}
		if v := cfg.Resolved()["load"].(map[string]any)["endpoint"]; v != "check" {
			t.Errorf("default resolved endpoint = %#v, want scalar \"check\"", v)
		}
	})

	t.Run("weighted mapping", func(t *testing.T) {
		cfg := writeAndLoad(t, "load:\n  endpoint:\n    check: 70\n    list-objects: 20\n    batch-check: 10\n")
		if _, ok := cfg.Load.Endpoint.Single(); ok {
			t.Fatal("blend should not report as a single endpoint")
		}
		if cfg.Load.Endpoint.Label() != "mixed" {
			t.Errorf("Label = %q, want mixed", cfg.Load.Endpoint.Label())
		}
		// Weights are sorted by name regardless of YAML order.
		want := []EndpointWeight{{"batch-check", 10}, {"check", 70}, {"list-objects", 20}}
		if len(cfg.Load.Endpoint.Weights) != len(want) {
			t.Fatalf("weights = %+v, want %+v", cfg.Load.Endpoint.Weights, want)
		}
		for i, w := range want {
			if cfg.Load.Endpoint.Weights[i] != w {
				t.Errorf("weight[%d] = %+v, want %+v", i, cfg.Load.Endpoint.Weights[i], w)
			}
		}
		// A blend round-trips as a mapping.
		v, ok := cfg.Resolved()["load"].(map[string]any)["endpoint"].(map[string]any)
		if !ok {
			t.Fatalf("resolved endpoint = %#v, want a mapping", cfg.Resolved()["load"].(map[string]any)["endpoint"])
		}
		if len(v) != 3 {
			t.Errorf("resolved endpoint mapping = %#v, want 3 entries", v)
		}
	})
}

// Invalid endpoint blends are rejected with clear errors.
func TestEndpointMixValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown endpoint", "load:\n  endpoint:\n    check: 1\n    bogus: 1\n", "must be check"},
		{"non-positive weight", "load:\n  endpoint:\n    check: 1\n    batch-check: 0\n", "weight"},
		{"negative weight", "load:\n  endpoint:\n    check: -1\n", "weight"},
		{"unknown scalar", "load:\n  endpoint: nope\n", "must be check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfigFile(path)
			if err == nil {
				t.Fatalf("expected %q to be rejected", c.yaml)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.want)
			}
		})
	}
}

// The weighted endpoint picker follows the configured shares; a single-endpoint
// mix returns the lone endpoint without consuming an rng draw (so single-endpoint
// corpus selection — and the determinism contract — is unchanged).
func TestEndpointPicker(t *testing.T) {
	t.Run("single consumes no draw", func(t *testing.T) {
		p := newEndpointPicker(singleEndpointMix("check"))
		a := rand.New(rand.NewSource(7))
		b := rand.New(rand.NewSource(7))
		for i := 0; i < 5; i++ {
			if got := p.pick(a); got != "check" {
				t.Fatalf("single picker returned %q, want check", got)
			}
		}
		// a never advanced, so it must still match a fresh same-seed rng.
		if a.Float64() != b.Float64() {
			t.Error("single-endpoint pick advanced the rng; corpus selection would shift")
		}
	})

	t.Run("weighted follows shares", func(t *testing.T) {
		mix := EndpointMix{Weights: []EndpointWeight{{"check", 75}, {"batch-check", 25}}}
		p := newEndpointPicker(mix)
		rng := rand.New(rand.NewSource(1))
		counts := map[string]int{}
		const n = 40000
		for i := 0; i < n; i++ {
			counts[p.pick(rng)]++
		}
		if frac := float64(counts["check"]) / n; frac < 0.72 || frac > 0.78 {
			t.Errorf("check share = %.3f, want ~0.75", frac)
		}
		if counts["check"]+counts["batch-check"] != n {
			t.Errorf("picker returned an unexpected endpoint: %+v", counts)
		}
	})
}

// A mixed-endpoint run exercises every selected endpoint at roughly its share
// and reports per-endpoint percentiles; the markdown gains the per-endpoint
// section.
func TestMixedEndpointRunReportsPerEndpoint(t *testing.T) {
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/batch-check"):
			var req BatchCheckRequest
			json.NewDecoder(r.Body).Decode(&req)
			result := map[string]any{}
			for _, item := range req.Checks {
				result[item.CorrelationID] = map[string]any{"allowed": true}
			}
			json.NewEncoder(w).Encode(map[string]any{"result": result})
		case strings.HasSuffix(r.URL.Path, "/list-objects"):
			json.NewEncoder(w).Encode(map[string]any{"objects": []string{"doc:1", "doc:2"}})
		case strings.HasSuffix(r.URL.Path, "/list-users"):
			json.NewEncoder(w).Encode(map[string]any{"users": []any{}})
		default:
			json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
		}
	})
	defer srv.Close()

	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Load.Endpoint = EndpointMix{Weights: []EndpointWeight{
		{"batch-check", 25}, {"check", 50}, {"list-objects", 25},
	}}
	cfg.Load.BatchSize = 3
	cfg.Load.Concurrency = 4
	cfg.Load.Warmup = 30 * time.Millisecond
	cfg.Load.Duration = 250 * time.Millisecond
	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true},
	}}

	res, err := RunLoad(client, corpus, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoint != "mixed" {
		t.Fatalf("LoadResult.Endpoint = %q, want mixed", res.Endpoint)
	}
	st := res.loadStats()
	for _, ep := range []string{"check", "batch-check", "list-objects"} {
		ls := st.byEndpoint[ep]
		if ls == nil || ls.Stats().Count == 0 {
			t.Errorf("endpoint %q saw no samples; mix did not exercise it", ep)
		}
	}

	r := BuildReport(res, corpus, cfg, 0, 0)
	if len(r.ByEndpoint) != 3 {
		t.Fatalf("ByEndpoint = %+v, want 3 endpoints", r.ByEndpoint)
	}
	if len(r.EndpointMix) != 3 {
		t.Fatalf("EndpointMix = %+v, want 3 shares", r.EndpointMix)
	}
	// Shares are normalized percentages summing to 100.
	var total float64
	for _, s := range r.EndpointMix {
		total += s.Share
	}
	if total < 99.5 || total > 100.5 {
		t.Errorf("endpoint shares sum to %.1f, want ~100", total)
	}
	md := r.Markdown()
	if !strings.Contains(md, "## Per-endpoint breakdown") {
		t.Error("markdown is missing the per-endpoint breakdown section")
	}
	if !strings.Contains(md, "mixed (") {
		t.Error("markdown config table did not render the endpoint blend")
	}

	// Digests must round-trip the per-endpoint split for distributed merge.
	if r.Digests == nil || len(r.Digests.ByEndpoint) != 3 {
		t.Fatalf("digests missing per-endpoint sketches: %+v", r.Digests)
	}
	data, err := json.Marshal(r.Digests)
	if err != nil {
		t.Fatal(err)
	}
	var back ReportDigests
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.loadStats().byEndpoint) != 3 {
		t.Errorf("per-endpoint digests did not survive a round trip: %+v", back.ByEndpoint)
	}
}

// A single-endpoint run must not emit any per-endpoint artifacts, keeping its
// report and digests byte-identical to before the blend knob existed.
func TestSingleEndpointOmitsPerEndpoint(t *testing.T) {
	st := newLoadStats()
	st.AddSample(Sample{Target: "doc#viewer", Endpoint: "check", Latency: time.Millisecond, Items: 1, ResultCount: -1})
	d := reportDigestsFromLoadStats(st, false)
	if d.ByEndpoint != nil {
		t.Errorf("single-endpoint digest carries ByEndpoint = %+v, want nil", d.ByEndpoint)
	}
}

func writeAndLoad(t *testing.T, yaml string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("loading %q: %v", yaml, err)
	}
	return cfg
}
