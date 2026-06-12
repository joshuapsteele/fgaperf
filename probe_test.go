package main

import (
	"fmt"
	"math/rand"
	"testing"
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
