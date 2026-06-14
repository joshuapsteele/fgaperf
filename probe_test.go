package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func corpusOf(target string, allowed, denied int) []CorpusEntry {
	var out []CorpusEntry
	for i := 0; i < allowed; i++ {
		out = append(out, CorpusEntry{Target: target, Expected: true})
	}
	for i := 0; i < denied; i++ {
		out = append(out, CorpusEntry{Target: target, Expected: false})
	}
	return out
}

func countAllowed(entries []CorpusEntry) (allowed, denied int) {
	for _, e := range entries {
		if e.Expected {
			allowed++
		} else {
			denied++
		}
	}
	return
}

func TestResampleBalancesEachTarget(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := append(corpusOf("a#r", 80, 20), corpusOf("b#r", 10, 90)...)

	out := resample(entries, 0.5, -1, rng)
	if len(out) != len(entries) {
		t.Fatalf("resample changed corpus size: got %d, want %d", len(out), len(entries))
	}
	byTarget := map[string][]CorpusEntry{}
	for _, e := range out {
		byTarget[e.Target] = append(byTarget[e.Target], e)
	}
	for target, group := range byTarget {
		allowed, denied := countAllowed(group)
		if allowed != 50 || denied != 50 {
			t.Errorf("target %s: got %d allowed / %d denied, want 50/50", target, allowed, denied)
		}
	}
}

func TestResampleNegativeRatioKeepsNaturalMix(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := corpusOf("a#r", 80, 20)

	out := resample(entries, -1, 5, rng)
	allowed, denied := countAllowed(out)
	if allowed != 80 || denied != 20 {
		t.Errorf("got %d/%d, want the natural 80/20 mix", allowed, denied)
	}
}

// A target where probing only ever saw one outcome cannot be rebalanced;
// resample must keep it rather than fabricate the missing class.
func TestResampleSingleOutcomeTargetKept(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := corpusOf("a#r", 40, 0)

	out := resample(entries, 0.5, 5, rng)
	allowed, denied := countAllowed(out)
	if allowed != 40 || denied != 0 {
		t.Errorf("got %d/%d, want all 40 allowed entries kept", allowed, denied)
	}
}

// When hitting allowed_ratio would replicate the scarce class beyond
// max_duplication, the target must keep its natural mix instead of producing
// a corpus of a few checks repeated dozens of times.
func TestResampleCapsDuplication(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := corpusOf("a#r", 5, 95) // ratio 0.5 would need 50 allowed from 5 distinct = 10x

	out := resample(entries, 0.5, 5, rng)
	allowed, denied := countAllowed(out)
	if allowed != 5 || denied != 95 {
		t.Errorf("got %d/%d, want the natural 5/95 mix preserved under the duplication cap", allowed, denied)
	}

	// Within the cap, rebalancing still happens.
	out = resample(entries, 0.5, 10, rng)
	allowed, denied = countAllowed(out)
	if allowed != 50 || denied != 50 {
		t.Errorf("got %d/%d, want 50/50 when duplication fits the cap", allowed, denied)
	}
}

// resample must be deterministic for a fixed seed: map-order iteration over
// targets would consume the RNG differently on every run.
func TestResampleDeterministic(t *testing.T) {
	entries := append(corpusOf("a#r", 30, 70), corpusOf("b#r", 70, 30)...)
	for i := range entries {
		entries[i].Object = fmt.Sprintf("o:%d", i) // make entries distinguishable
	}
	out1 := resample(entries, 0.5, 5, rand.New(rand.NewSource(7)))
	out2 := resample(entries, 0.5, 5, rand.New(rand.NewSource(7)))
	if len(out1) != len(out2) {
		t.Fatalf("lengths differ: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].key() != out2[i].key() || out1[i].Expected != out2[i].Expected {
			t.Fatalf("entry %d differs: %+v vs %+v", i, out1[i], out2[i])
		}
	}
}

func TestCorpusDuplicationStats(t *testing.T) {
	c := &Corpus{Entries: []CorpusEntry{
		{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1"},
		{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1"}, // duplicate
		{Target: "a#r", User: "user:2", Relation: "r", Object: "a:1"},
		{Target: "b#r", User: "user:1", Relation: "r", Object: "b:1"},
	}}
	if got := c.Distinct(); got != 3 {
		t.Errorf("Distinct() = %d, want 3", got)
	}
	stats := c.TargetStats()
	if st := stats["a#r"]; st.Total != 3 || st.Distinct != 2 {
		t.Errorf("a#r stats = %+v, want total 3 distinct 2", st)
	}
	if st := stats["b#r"]; st.Total != 1 || st.Distinct != 1 {
		t.Errorf("b#r stats = %+v, want total 1 distinct 1", st)
	}
}

func TestCorpusDistinctIncludesRequestContext(t *testing.T) {
	c := &Corpus{Entries: []CorpusEntry{
		{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1", Context: map[string]any{"scope": "read"}},
		{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1", Context: map[string]any{"scope": "write"}},
		{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1", ContextualTuples: []TupleKey{{
			User:     "user:1",
			Relation: "active_context",
			Object:   "a:1",
		}}},
	}}
	if got := c.Distinct(); got != 3 {
		t.Fatalf("Distinct() = %d, want 3 request identities", got)
	}
	if st := c.TargetStats()["a#r"]; st.Total != 3 || st.Distinct != 3 {
		t.Fatalf("stats = %+v, want total 3 distinct 3", st)
	}
}

func TestContextualTuplesUseSameObjectWhenTypesMatch(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	cfg.Contextual.Relations = []string{"document#active_context"}
	w := NewWorld(a, cfg)
	rels, err := contextualRelations(a, cfg)
	if err != nil {
		t.Fatal(err)
	}

	tuples := w.ContextualTuples(rels, nil, "user:anne", "document:doc1", rand.New(rand.NewSource(1)), 1)
	if len(tuples) != 1 {
		t.Fatalf("got %d contextual tuples, want 1: %+v", len(tuples), tuples)
	}
	if tuples[0].User != "user:anne" || tuples[0].Relation != "active_context" || tuples[0].Object != "document:doc1" {
		t.Fatalf("unexpected contextual tuple: %+v", tuples[0])
	}
}

func TestContextualTuplesPreferSeededRelatedObject(t *testing.T) {
	cfg, _ := LoadConfigFile("")
	w := &World{
		Instances: map[string][]string{
			"customer": []string{"customer:other", "customer:chosen"},
		},
		Cohort: map[string]int{
			"policy:p1":       0,
			"customer:chosen": 0,
			"customer:other":  0,
		},
		cfg: cfg,
		rng: rand.New(rand.NewSource(1)),
	}
	idx := NewTupleIndex([]TupleKey{
		{User: "customer:chosen", Relation: "customer", Object: "policy:p1"},
	})
	rels := []contextualRelation{{
		ObjectType: "customer",
		Relation:   "principal_in_context",
		Refs:       []RelationReference{{Type: "user"}},
	}}

	tuples := w.ContextualTuples(rels, idx, "user:anne", "policy:p1", rand.New(rand.NewSource(1)), 1)
	if len(tuples) != 1 {
		t.Fatalf("got %d contextual tuples, want 1: %+v", len(tuples), tuples)
	}
	if tuples[0].Object != "customer:chosen" {
		t.Fatalf("contextual tuple did not use seeded related customer: %+v", tuples[0])
	}
}

// attributeDatastoreQueries must isolate a per-relation datastore cost: one
// relation at a time, distinct checks only, dividing the server's
// datastore-query histogram diff by the recorded count.
func TestAttributeDatastoreQueries(t *testing.T) {
	// Datastore queries the fake server "performs" per check, by relation: a
	// deep relation costs many reads, a direct one a few — the sanity ordering
	// the acceptance criteria call for.
	cost := map[string]int{"viewer": 2, "editor": 12}
	var mu sync.Mutex
	var sum, count int
	perRel := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metrics") {
			mu.Lock()
			s, c := sum, count
			mu.Unlock()
			// One aggregated series, as the real (label-summed) metric appears.
			fmt.Fprint(w, "# TYPE openfga_datastore_query_count histogram\n")
			fmt.Fprintf(w, "openfga_datastore_query_count_bucket{le=\"+Inf\"} %d\n", c)
			fmt.Fprintf(w, "openfga_datastore_query_count_sum %d\n", s)
			fmt.Fprintf(w, "openfga_datastore_query_count_count %d\n", c)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/check") {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var req CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding check: %v", err)
			return
		}
		if req.Consistency != "HIGHER_CONSISTENCY" {
			t.Errorf("attribution check used consistency %q, want HIGHER_CONSISTENCY", req.Consistency)
		}
		mu.Lock()
		sum += cost[req.TupleKey.Relation]
		count++
		perRel[req.TupleKey.Relation]++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	}))
	defer srv.Close()

	client := NewFGAClient(OpenFGAConfig{APIURL: srv.URL, Timeout: 5 * time.Second}, 4)
	scraper := NewMetricsScraper(srv.URL)

	corpus := &Corpus{Entries: []CorpusEntry{
		{Target: "document#viewer", User: "user:1", Relation: "viewer", Object: "document:1"},
		{Target: "document#viewer", User: "user:2", Relation: "viewer", Object: "document:2"},
		{Target: "document#viewer", User: "user:1", Relation: "viewer", Object: "document:1"}, // duplicate: must be skipped
		{Target: "document#editor", User: "user:3", Relation: "editor", Object: "document:5"},
		{Target: "document#editor", User: "user:4", Relation: "editor", Object: "document:6"},
	}}
	corpus.Stats = corpus.TargetStats()

	attributeDatastoreQueries(client, scraper, "store1", "model1", corpus)

	if got := corpus.DSQueries["document#viewer"]; got != 2 {
		t.Errorf("viewer DS queries/check = %v, want 2", got)
	}
	if got := corpus.DSQueries["document#editor"]; got != 12 {
		t.Errorf("editor DS queries/check = %v, want 12", got)
	}
	if corpus.DSQueries["document#editor"] <= corpus.DSQueries["document#viewer"] {
		t.Errorf("expected the deeper editor relation to cost more datastore reads than viewer: %+v", corpus.DSQueries)
	}
	// Distinct only: 2 viewer (the dup dropped) + 2 editor = 4 checks total.
	if count != 4 {
		t.Errorf("ran %d attribution checks, want 4 (distinct entries only)", count)
	}
	if perRel["viewer"] != 2 {
		t.Errorf("ran %d viewer checks, want 2 distinct", perRel["viewer"])
	}
}

// Attribution is best-effort: an unreachable/erroring metrics endpoint must
// leave DSQueries unset rather than fail the probe.
func TestAttributeDatastoreQueriesBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metrics") {
			http.Error(w, "metrics down", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	}))
	defer srv.Close()

	client := NewFGAClient(OpenFGAConfig{APIURL: srv.URL, Timeout: 5 * time.Second}, 4)
	scraper := NewMetricsScraper(srv.URL)
	corpus := &Corpus{Entries: []CorpusEntry{
		{Target: "document#viewer", User: "user:1", Relation: "viewer", Object: "document:1"},
	}}
	corpus.Stats = corpus.TargetStats()

	attributeDatastoreQueries(client, scraper, "store1", "model1", corpus)
	if corpus.DSQueries != nil {
		t.Errorf("expected no attribution when metrics endpoint errors, got %+v", corpus.DSQueries)
	}
}

func TestChurnTemplatesArePlainAndPreferTerminal(t *testing.T) {
	a := loadExampleModel(t)
	templates := churnTemplates(a)
	if len(templates) == 0 {
		t.Fatal("example model produced no churn templates; it has plain user-type relations")
	}
	for _, tpl := range templates {
		// Every template must correspond to a direct ref that is plain: no
		// userset, no wildcard, no condition. Anything else could make churn
		// tuples reachable from corpus checks and shift ground truth.
		found := false
		for _, ref := range a.DirectRefs[tpl.ObjectType][tpl.Relation] {
			if ref.Type == tpl.UserType && ref.Relation == "" && ref.Wildcard == nil && ref.Condition == "" {
				found = true
			}
		}
		if !found {
			t.Errorf("template %+v does not match a plain direct ref in the model", tpl)
		}
		// The example model's only terminal subject type is "user", and it has
		// plain user refs, so the terminal-preferred branch must win.
		if tpl.UserType != "user" {
			t.Errorf("template %+v: want terminal user type %q, got %q", tpl, "user", tpl.UserType)
		}
	}
}
