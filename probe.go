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
	User             string         `json:"user"`
	Relation         string         `json:"relation"`
	Object           string         `json:"object"`
	ContextualTuples []TupleKey     `json:"contextual_tuples,omitempty"`
	Context          map[string]any `json:"context,omitempty"`
	Expected         bool           `json:"expected"`
	Target           string         `json:"target"`      // "type#relation"
	Conditioned      bool           `json:"conditioned"` // CEL possibly on the resolution path
	Contextual       bool           `json:"contextual"`  // request carries contextual tuples
}

type Corpus struct {
	StoreID string        `json:"store_id"`
	ModelID string        `json:"model_id"`
	Entries []CorpusEntry `json:"entries"`
}

func BuildCorpus(client *FGAClient, a *Analysis, w *World, cfg *Config, storeID, modelID string) (*Corpus, error) {
	contextual, err := contextualRelations(a, cfg)
	if err != nil {
		return nil, err
	}
	attachProbability := contextualAttachProbability(cfg)
	if attachProbability < 0 || attachProbability > 1 {
		return nil, fmt.Errorf("contextual.attach_probability must be between 0 and 1")
	}
	var tupleIndex *TupleIndex
	if len(contextual) > 0 {
		tupleIndex = NewTupleIndex(w.GenerateTuples())
	}

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
			contextualTuples := w.ContextualTuples(contextual, tupleIndex, subj, obj, rng, attachProbability)
			candidates = append(candidates, CorpusEntry{
				User:             subj,
				Relation:         relation,
				Object:           obj,
				ContextualTuples: contextualTuples,
				Context:          w.RequestContext(rng),
				Target:           target,
				Conditioned:      a.Conditioned[target],
				Contextual:       len(contextualTuples) > 0,
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
				ContextualTuples:     contextualTupleKeys(candidates[i].ContextualTuples),
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

type contextualRelation struct {
	ObjectType string
	Relation   string
	Refs       []RelationReference
}

func contextualRelations(a *Analysis, cfg *Config) ([]contextualRelation, error) {
	out := make([]contextualRelation, 0, len(cfg.Contextual.Relations))
	for _, key := range cfg.Contextual.Relations {
		parts := strings.SplitN(key, "#", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad contextual relation %q, want type#relation", key)
		}
		refs := a.DirectRefs[parts[0]][parts[1]]
		if len(refs) == 0 {
			return nil, fmt.Errorf("contextual relation %q does not accept direct tuples", key)
		}
		out = append(out, contextualRelation{ObjectType: parts[0], Relation: parts[1], Refs: refs})
	}
	return out, nil
}

func contextualAttachProbability(cfg *Config) float64 {
	if cfg.Contextual.AttachProbability == nil {
		return 1
	}
	return *cfg.Contextual.AttachProbability
}

func contextualTupleKeys(tuples []TupleKey) *ContextualTupleKeys {
	if len(tuples) == 0 {
		return nil
	}
	return &ContextualTupleKeys{TupleKeys: tuples}
}

func (w *World) ContextualTuples(relations []contextualRelation, idx *TupleIndex, user, object string, rng *rand.Rand, attachProbability float64) []TupleKey {
	if len(relations) == 0 || rng.Float64() > attachProbability {
		return nil
	}
	subjectType := typeOfUser(user)
	objectType := typeOfObject(object)
	cohort, hasCohort := w.Cohort[object]
	seen := map[string]bool{}
	var out []TupleKey
	for _, rel := range relations {
		ref, ok := rel.matchingRef(subjectType)
		if !ok {
			continue
		}
		ctxObject := object
		if rel.ObjectType != objectType {
			related := idx.RelatedObjects(object, rel.ObjectType)
			if len(related) > 0 {
				ctxObject = related[rng.Intn(len(related))]
			} else if hasCohort {
				ctxObject = w.pickInCohort(rel.ObjectType, cohort)
			} else {
				ctxObject = ""
			}
		}
		if ctxObject == "" {
			continue
		}
		t := TupleKey{
			User:      user,
			Relation:  rel.Relation,
			Object:    ctxObject,
			Condition: w.tupleCondition(ref.Condition),
		}
		key := t.User + "|" + t.Relation + "|" + t.Object
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

func (r contextualRelation) matchingRef(subjectType string) (RelationReference, bool) {
	for _, ref := range r.Refs {
		if ref.Type == subjectType && ref.Relation == "" && ref.Wildcard == nil {
			return ref, true
		}
	}
	return RelationReference{}, false
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
