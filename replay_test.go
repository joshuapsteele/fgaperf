package main

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseReplayLogDedupsAndCounts(t *testing.T) {
	log := `
{"user":"user:1","relation":"viewer","object":"document:1"}
{"user":"user:1","relation":"viewer","object":"document:1"}
{"user":"user:2","relation":"viewer","object":"document:1"}
{"user":"user:1","relation":"editor","object":"document:9"}
`
	conditioned := map[string]bool{"document#viewer": true}
	parsed, err := parseReplayLog(strings.NewReader(log), conditioned)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.total != 4 {
		t.Errorf("total = %d, want 4 well-formed lines", parsed.total)
	}
	if parsed.skipped != 0 {
		t.Errorf("skipped = %d, want 0", parsed.skipped)
	}
	// Three distinct identities: (u1,viewer,d1), (u2,viewer,d1), (u1,editor,d9).
	if len(parsed.distinct) != 3 {
		t.Fatalf("distinct = %d, want 3: %+v", len(parsed.distinct), parsed.distinct)
	}
	// Weights are natural per-target counts, not distinct counts: viewer saw 3
	// lines, editor saw 1.
	if parsed.weights["document#viewer"] != 3 || parsed.weights["document#editor"] != 1 {
		t.Errorf("weights = %+v, want viewer 3 / editor 1", parsed.weights)
	}
	// Conditioned tag flows from the analysis map onto every entry of that target.
	for _, e := range parsed.distinct {
		want := conditioned[e.Target]
		if e.Conditioned != want {
			t.Errorf("entry %s#%s conditioned = %v, want %v", e.Target, e.User, e.Conditioned, want)
		}
	}
}

func TestParseReplayLogContextAndContextual(t *testing.T) {
	log := `{"user":"user:1","relation":"viewer","object":"document:1","context":{"scope":"read"},"contextual_tuples":[{"user":"user:1","relation":"member","object":"group:g1"}]}
{"user":"user:1","relation":"viewer","object":"document:1","context":{"scope":"write"}}`
	parsed, err := parseReplayLog(strings.NewReader(log), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Differing context maps make these two distinct request identities.
	if len(parsed.distinct) != 2 {
		t.Fatalf("distinct = %d, want 2 (context differs)", len(parsed.distinct))
	}
	var withCtxTuples *CorpusEntry
	for i := range parsed.distinct {
		if len(parsed.distinct[i].ContextualTuples) > 0 {
			withCtxTuples = &parsed.distinct[i]
		}
	}
	if withCtxTuples == nil {
		t.Fatal("no entry carried contextual tuples")
	}
	if !withCtxTuples.Contextual {
		t.Error("entry with contextual_tuples should be tagged Contextual")
	}
	if got := withCtxTuples.ContextualTuples[0]; got.Relation != "member" || got.Object != "group:g1" {
		t.Errorf("contextual tuple = %+v, want member@group:g1", got)
	}
}

func TestParseReplayLogSkipsMalformed(t *testing.T) {
	log := `{"user":"user:1","relation":"viewer","object":"document:1"}

not json at all
{"user":"user:2","relation":"viewer"}
{"user":"user:3","relation":"viewer","object":"noprefix"}
{"user":"user:4","relation":"viewer","object":"document:2"}`
	parsed, err := parseReplayLog(strings.NewReader(log), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two valid lines (d1, d2); three skipped (bad JSON, missing object, no
	// type prefix); the blank line is ignored, not skipped.
	if parsed.total != 2 {
		t.Errorf("total = %d, want 2", parsed.total)
	}
	if parsed.skipped != 3 {
		t.Errorf("skipped = %d, want 3", parsed.skipped)
	}
	if len(parsed.distinct) != 2 {
		t.Errorf("distinct = %d, want 2", len(parsed.distinct))
	}
	if len(parsed.errSamples) == 0 {
		t.Error("expected at least one skip-reason sample for the operator")
	}
}

func TestParseReplayLogSkipsOversizedLineAndContinues(t *testing.T) {
	oversized := strings.Repeat("x", maxReplayLineBytes+1)
	log := oversized + "\n" + `{"user":"user:1","relation":"viewer","object":"document:1"}`
	parsed, err := parseReplayLog(strings.NewReader(log), nil)
	if err != nil {
		t.Fatalf("oversized replay line should be skipped, not fatal: %v", err)
	}
	if parsed.skipped != 1 {
		t.Errorf("skipped = %d, want 1 oversized line", parsed.skipped)
	}
	if parsed.total != 1 || len(parsed.distinct) != 1 {
		t.Fatalf("valid line after oversized line was not parsed: total=%d distinct=%d", parsed.total, len(parsed.distinct))
	}
	if len(parsed.errSamples) == 0 || !strings.Contains(parsed.errSamples[0], "exceeds") {
		t.Errorf("oversized skip reason not retained: %v", parsed.errSamples)
	}
}

func TestParseReplayLogIgnoresUnknownFields(t *testing.T) {
	// A raw OpenFGA request log carries far more than the four fields we read;
	// the extras must be ignored, not rejected.
	log := `{"store_id":"01ABC","authorization_model_id":"01XYZ","timestamp":"2026-06-14T00:00:00Z","consistency":"MINIMIZE_LATENCY","user":"user:1","relation":"viewer","object":"document:1"}`
	parsed, err := parseReplayLog(strings.NewReader(log), nil)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.total != 1 || len(parsed.distinct) != 1 {
		t.Fatalf("total %d distinct %d, want 1/1 (unknown fields ignored)", parsed.total, len(parsed.distinct))
	}
	e := parsed.distinct[0]
	if e.User != "user:1" || e.Relation != "viewer" || e.Object != "document:1" {
		t.Errorf("parsed entry = %+v, want user:1/viewer/document:1", e)
	}
}

// The whole point of replay is that the load mix follows the log's per-target
// distribution. With natural-count weights, corpusPicker must pick targets in
// proportion to those counts — not uniformly over corpus entries, which would
// over-represent a target that happens to have more distinct checks.
func TestReplayWeightsDriveTargetMix(t *testing.T) {
	// viewer: 2 distinct entries, weight 800. editor: 8 distinct entries,
	// weight 200. Uniform-over-entries would give viewer 2/10=20%; weighting
	// must give viewer 800/1000=80%.
	var entries []CorpusEntry
	for i := 0; i < 2; i++ {
		entries = append(entries, CorpusEntry{Target: "document#viewer", Object: "document:v", User: itoaUser(i)})
	}
	for i := 0; i < 8; i++ {
		entries = append(entries, CorpusEntry{Target: "document#editor", Object: "document:e", User: itoaUser(i)})
	}
	c := &Corpus{
		Entries: entries,
		Weights: map[string]float64{"document#viewer": 800, "document#editor": 200},
	}
	picker := newCorpusPicker(c)
	rng := rand.New(rand.NewSource(1))
	counts := map[string]int{}
	const n = 200000
	for i := 0; i < n; i++ {
		counts[picker.pick(rng).Target]++
	}
	viewerShare := float64(counts["document#viewer"]) / float64(n)
	if math.Abs(viewerShare-0.8) > 0.02 {
		t.Errorf("viewer share = %.3f, want ~0.80 (weighted by log counts, not entry counts)", viewerShare)
	}
}

func itoaUser(i int) string {
	return "user:" + string(rune('a'+i))
}

// BuildReplayCorpus end to end against a fake server: it must learn each
// distinct entry's ground truth (one HIGHER_CONSISTENCY check apiece), drop
// errored entries, skip malformed lines, and weight the corpus by the log's
// per-target counts.
func TestBuildReplayCorpus(t *testing.T) {
	a := loadExampleModel(t)

	// Fake check endpoint: allow when the object id is even, deny when odd, so
	// outcomes are deterministic and we can assert on them. The handler runs on
	// many goroutines (probe.concurrency workers), so shared state is atomic and
	// failures are reported with the concurrency-safe t.Errorf, never t.Fatalf.
	var checks atomic.Int64
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/check") {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var req CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding check: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Consistency != "HIGHER_CONSISTENCY" {
			t.Errorf("ground-truth check used consistency %q, want HIGHER_CONSISTENCY", req.Consistency)
		}
		checks.Add(1)
		allowed := strings.HasSuffix(req.TupleKey.Object, "0") || strings.HasSuffix(req.TupleKey.Object, "2")
		json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
	})
	defer srv.Close()

	// document:2 -> allowed (x2 lines, one duplicate), document:1 -> denied,
	// document:3 -> denied (editor). Plus a malformed line that must be skipped.
	log := `{"user":"user:1","relation":"viewer","object":"document:2"}
{"user":"user:1","relation":"viewer","object":"document:2"}
{"user":"user:9","relation":"viewer","object":"document:1"}
garbage
{"user":"user:1","relation":"editor","object":"document:3"}`
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _ := LoadConfigFile("")
	cfg.CorpusSource = "replay"
	cfg.Replay.File = path

	corpus, err := BuildReplayCorpus(client, a, cfg, "store1", "model1")
	if err != nil {
		t.Fatal(err)
	}
	// Three distinct checks classified (the duplicate document:2 line is one).
	if got := checks.Load(); got != 3 {
		t.Errorf("ran %d ground-truth checks, want 3 (distinct entries only)", got)
	}
	if len(corpus.Entries) != 3 {
		t.Fatalf("corpus has %d entries, want 3: %+v", len(corpus.Entries), corpus.Entries)
	}
	if corpus.StoreID != "store1" || corpus.ModelID != "model1" {
		t.Errorf("corpus store/model = %q/%q", corpus.StoreID, corpus.ModelID)
	}
	// Weights are the log's natural counts: viewer saw 3 lines, editor 1.
	if corpus.Weights["document#viewer"] != 3 || corpus.Weights["document#editor"] != 1 {
		t.Errorf("weights = %+v, want viewer 3 / editor 1", corpus.Weights)
	}
	// Ground truth recorded per entry.
	for _, e := range corpus.Entries {
		want := strings.HasSuffix(e.Object, "2")
		if e.Expected != want {
			t.Errorf("entry %s expected %v, want %v", e.Object, e.Expected, want)
		}
	}
	// The example model has plain user-type relations, so churn templates carry over.
	if len(corpus.ChurnTemplates) == 0 {
		t.Error("expected churn templates from the example model")
	}
}

func TestReplayConfigValidation(t *testing.T) {
	// corpus_source: replay requires replay.file.
	cfg := &Config{}
	cfg.applyDefaults(nil)
	cfg.CorpusSource = "replay"
	if err := cfg.validate(); err == nil {
		t.Error("expected error: replay without replay.file")
	}
	cfg.Replay.File = "log.jsonl"
	if err := cfg.validate(); err != nil {
		t.Errorf("replay with a file should validate: %v", err)
	}

	// An unknown corpus_source is rejected.
	cfg2 := &Config{}
	cfg2.applyDefaults(nil)
	cfg2.CorpusSource = "bogus"
	if err := cfg2.validate(); err == nil {
		t.Error("expected error: unknown corpus_source")
	}

	// Default is probe.
	cfg3 := &Config{}
	cfg3.applyDefaults(nil)
	if cfg3.CorpusSource != "probe" {
		t.Errorf("default corpus_source = %q, want probe", cfg3.CorpusSource)
	}
}
