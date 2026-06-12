package main

// probe.go builds the check corpus. Rather than trying to statically predict
// which (subject, relation, object) triples resolve to allowed (hard in the
// presence of intersections and conditions), we sample candidates, execute
// each once at low concurrency, and record the actual outcome. The load phase
// then replays a corpus with a known allowed/denied mix, and can verify that
// results under load match the probe-time expectations.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
)

type CorpusEntry struct {
	User        string         `json:"user"`
	Relation    string         `json:"relation"`
	Object      string         `json:"object"`
	Context     map[string]any `json:"context,omitempty"`
	Expected    bool           `json:"expected"`
	Target      string         `json:"target"`      // "type#relation"
	Conditioned bool           `json:"conditioned"` // CEL possibly on the resolution path
}

type Corpus struct {
	StoreID string        `json:"store_id"`
	ModelID string        `json:"model_id"`
	Entries []CorpusEntry `json:"entries"`
}

func BuildCorpus(client *FGAClient, a *Analysis, w *World, cfg *Config, storeID, modelID string) (*Corpus, error) {
	targets := cfg.Probe.Targets
	if len(targets) == 0 {
		for _, tr := range a.AllRelations {
			targets = append(targets, tr.Key())
		}
	}
	subjectTypes := cfg.Probe.SubjectTypes
	if len(subjectTypes) == 0 {
		subjectTypes = a.SubjectTypes
	}
	if len(subjectTypes) == 0 {
		return nil, fmt.Errorf("no subject types inferred from model; set probe.subject_types")
	}

	rng := rand.New(rand.NewSource(cfg.RandomSeed + 1))
	var candidates []CorpusEntry
	for _, target := range targets {
		parts := strings.SplitN(target, "#", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad target %q, want type#relation", target)
		}
		objType, relation := parts[0], parts[1]
		objects := w.Instances[objType]
		if len(objects) == 0 {
			continue
		}
		for i := 0; i < cfg.Probe.Samples; i++ {
			obj := objects[rng.Intn(len(objects))]
			st := subjectTypes[rng.Intn(len(subjectTypes))]
			var subj string
			if rng.Float64() < cfg.Probe.CohortBias {
				subj = w.pickInCohort(st, w.Cohort[obj])
			}
			if subj == "" {
				pool := w.Instances[st]
				if len(pool) == 0 {
					continue
				}
				subj = pool[rng.Intn(len(pool))]
			}
			candidates = append(candidates, CorpusEntry{
				User:        subj,
				Relation:    relation,
				Object:      obj,
				Context:     w.RequestContext(rng),
				Target:      target,
				Conditioned: a.Conditioned[target],
			})
		}
	}

	// Classify candidates with bounded concurrency.
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Probe.Concurrency)
	errs := make([]error, len(candidates))
	for i := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			allowed, err := client.Check(storeID, CheckRequest{
				TupleKey: CheckTupleKey{
					User:     candidates[i].User,
					Relation: candidates[i].Relation,
					Object:   candidates[i].Object,
				},
				Context:              candidates[i].Context,
				AuthorizationModelID: modelID,
				Consistency:          "HIGHER_CONSISTENCY",
			})
			if err != nil {
				errs[i] = err
				return
			}
			candidates[i].Expected = allowed
		}(i)
	}
	wg.Wait()

	var valid []CorpusEntry
	var errCount int
	for i, e := range errs {
		if e != nil {
			errCount++
			if errCount <= 3 {
				fmt.Fprintf(os.Stderr, "probe error: %v\n", e)
			}
			continue
		}
		valid = append(valid, candidates[i])
	}
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "probe: %d/%d candidates errored and were dropped\n", errCount, len(candidates))
	}

	entries := resample(valid, cfg.Probe.AllowedRatio, rng)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Target < entries[j].Target })
	return &Corpus{StoreID: storeID, ModelID: modelID, Entries: entries}, nil
}

// resample rebalances the corpus per target toward the requested allowed
// ratio, duplicating entries from the scarcer class when needed. A negative
// ratio keeps the natural mix.
func resample(entries []CorpusEntry, ratio float64, rng *rand.Rand) []CorpusEntry {
	if ratio < 0 {
		return entries
	}
	byTarget := map[string][]CorpusEntry{}
	for _, e := range entries {
		byTarget[e.Target] = append(byTarget[e.Target], e)
	}
	var out []CorpusEntry
	for target, group := range byTarget {
		var allowed, denied []CorpusEntry
		for _, e := range group {
			if e.Expected {
				allowed = append(allowed, e)
			} else {
				denied = append(denied, e)
			}
		}
		if len(allowed) == 0 || len(denied) == 0 {
			fmt.Fprintf(os.Stderr, "probe: target %s has only %s outcomes (allowed=%d denied=%d); keeping natural mix\n",
				target, map[bool]string{true: "allowed", false: "denied"}[len(allowed) > 0], len(allowed), len(denied))
			out = append(out, group...)
			continue
		}
		n := len(group)
		wantAllowed := int(float64(n) * ratio)
		wantDenied := n - wantAllowed
		out = append(out, sampleN(allowed, wantAllowed, rng)...)
		out = append(out, sampleN(denied, wantDenied, rng)...)
	}
	return out
}

func sampleN(src []CorpusEntry, n int, rng *rand.Rand) []CorpusEntry {
	out := make([]CorpusEntry, n)
	for i := 0; i < n; i++ {
		out[i] = src[rng.Intn(len(src))]
	}
	return out
}

func (c *Corpus) Save(path string) error {
	data, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
