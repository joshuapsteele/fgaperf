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
