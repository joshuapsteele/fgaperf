package main

import (
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

	out := resample(entries, 0.5, rng)
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

	out := resample(entries, -1, rng)
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

	out := resample(entries, 0.5, rng)
	allowed, denied := countAllowed(out)
	if allowed != 40 || denied != 0 {
		t.Errorf("got %d/%d, want all 40 allowed entries kept", allowed, denied)
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
