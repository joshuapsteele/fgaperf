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
	StoreID string                       `json:"store_id"`
	ModelID string                       `json:"model_id"`
	Stats   map[string]CorpusTargetStats `json:"target_stats,omitempty"`
	Weights map[string]float64           `json:"weights,omitempty"` // load traffic share per target; absent = uniform over entries
	// ChurnTemplates are (object type, relation, user type) shapes for
	// background-write churn (load.write_rate). Derived from the model at
	// probe time because `run` never loads the model.
	ChurnTemplates []ChurnTemplate `json:"churn_templates,omitempty"`
	// DSQueries is the per-target mean datastore queries per check, attributed
	// at probe time by attributeDatastoreQueries when probe.attribute_ds_queries
	// is set and a metrics endpoint is configured. Absent (and the report column
	// omitted) otherwise; its absence keeps the default corpus byte-identical.
	DSQueries map[string]float64 `json:"ds_queries_per_check,omitempty"`
	Entries   []CorpusEntry      `json:"entries"`
}

// ChurnTemplate is a relation shape safe for background churn: it accepts a
// plain (non-userset, non-wildcard) user type with no condition. Churn tuples
// instantiate these with fresh churn-only instance IDs, so they are
// unreachable from every corpus check: no wildcard can pull other subjects
// in, and no seeded object ever references a churn instance — corpus ground
// truth cannot shift, keeping verify_results meaningful.
type ChurnTemplate struct {
	ObjectType string `json:"object_type"`
	Relation   string `json:"relation"`
	UserType   string `json:"user_type"`
}

// churnTemplates collects safe churn shapes, preferring terminal subject
// types when any exist.
func churnTemplates(a *Analysis) []ChurnTemplate {
	terminal := map[string]bool{}
	for _, t := range a.SubjectTypes {
		terminal[t] = true
	}
	var preferred, fallback []ChurnTemplate
	for _, tr := range a.AllRelations {
		for _, ref := range a.DirectRefs[tr.Type][tr.Relation] {
			if ref.Relation != "" || ref.Wildcard != nil || ref.Condition != "" {
				continue
			}
			t := ChurnTemplate{ObjectType: tr.Type, Relation: tr.Relation, UserType: ref.Type}
			if terminal[ref.Type] {
				preferred = append(preferred, t)
			} else {
				fallback = append(fallback, t)
			}
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return fallback
}

// CorpusTargetStats records how much resampling duplicated a target's entries.
// A low distinct count means the load phase replays few unique checks, which
// inflates server cache hit rates relative to real traffic.
type CorpusTargetStats struct {
	Total    int `json:"total"`
	Distinct int `json:"distinct"`
}

func (e CorpusEntry) key() string {
	data, err := json.Marshal(struct {
		User             string         `json:"user"`
		Relation         string         `json:"relation"`
		Object           string         `json:"object"`
		ContextualTuples []TupleKey     `json:"contextual_tuples,omitempty"`
		Context          map[string]any `json:"context,omitempty"`
	}{
		User:             e.User,
		Relation:         e.Relation,
		Object:           e.Object,
		ContextualTuples: e.ContextualTuples,
		Context:          e.Context,
	})
	if err != nil {
		return e.User + "|" + e.Relation + "|" + e.Object
	}
	return string(data)
}

// TargetStats computes total and distinct request identities per target.
// Derived from the entries so it is always consistent with them.
func (c *Corpus) TargetStats() map[string]CorpusTargetStats {
	seen := map[string]map[string]bool{}
	out := map[string]CorpusTargetStats{}
	for _, e := range c.Entries {
		if seen[e.Target] == nil {
			seen[e.Target] = map[string]bool{}
		}
		seen[e.Target][e.key()] = true
		st := out[e.Target]
		st.Total++
		st.Distinct = len(seen[e.Target])
		out[e.Target] = st
	}
	return out
}

// Distinct counts unique request identities across the corpus.
func (c *Corpus) Distinct() int {
	seen := map[string]bool{}
	for _, e := range c.Entries {
		seen[e.key()] = true
	}
	return len(seen)
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
			targets = append(targets, TargetSpec{Relation: tr.Key(), Weight: 1})
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
	for _, spec := range targets {
		target := spec.Relation
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

	valid := classifyCandidates(client, storeID, modelID, candidates, cfg.Probe.Concurrency, "probe")

	entries := resample(valid, cfg.Probe.AllowedRatio, cfg.Probe.MaxDuplication, rng)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Target < entries[j].Target })
	c := &Corpus{StoreID: storeID, ModelID: modelID, Entries: entries}
	c.Stats = c.TargetStats()
	// Persist weights only when configured: their absence keeps the load
	// phase's original uniform-over-entries behavior.
	weighted := false
	weights := map[string]float64{}
	for _, spec := range targets {
		weights[spec.Relation] = spec.Weight
		if spec.Weight != 1 {
			weighted = true
		}
	}
	if weighted {
		c.Weights = weights
	}
	c.ChurnTemplates = churnTemplates(a)
	return c, nil
}

// classifyCandidates executes each candidate once at HIGHER_CONSISTENCY to
// learn its ground-truth outcome, with bounded concurrency, and returns the
// candidates that did not error with Expected filled in. Errored candidates
// are reported (first few verbatim) and dropped. This is the empirical core of
// principle #2 (probe-then-replay): outcomes are learned by executing checks,
// never predicted statically. Both the synthesized-corpus (BuildCorpus) and
// real-log (BuildReplayCorpus) builders share it; phase names it in messages.
func classifyCandidates(client *FGAClient, storeID, modelID string, candidates []CorpusEntry, concurrency int, phase string) []CorpusEntry {
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	errs := make([]error, len(candidates))
	progress := newProbeProgress(len(candidates))
	stopProgress := make(chan struct{})
	progressDone := make(chan struct{})
	if progress != nil {
		go progress.run(stopProgress, progressDone)
	} else {
		close(progressDone)
	}
	for i := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			progress.setCurrent(candidates[i].Target)
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
				progress.add(false, false)
				return
			}
			candidates[i].Expected = allowed
			progress.add(allowed, true)
		}(i)
	}
	wg.Wait()
	close(stopProgress)
	<-progressDone

	var valid []CorpusEntry
	var errCount int
	for i, e := range errs {
		if e != nil {
			errCount++
			if errCount <= 3 {
				fmt.Fprintf(os.Stderr, "%s error: %v\n", phase, e)
			}
			continue
		}
		valid = append(valid, candidates[i])
	}
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d/%d candidates errored and were dropped\n", phase, errCount, len(candidates))
	}
	return valid
}

const (
	// attributeBatchSize bounds how many distinct checks per target the DS
	// attribution pass replays — enough to average over a target's
	// allowed/denied mix without materially lengthening probe.
	attributeBatchSize = 50
	// attributeConcurrency keeps each per-target batch at low concurrency, so
	// its datastore-query diff windows a short, mostly-isolated burst of
	// traffic (one relation at a time is what makes the attribution per-relation).
	attributeConcurrency = 4
)

// attributeDatastoreQueries estimates, per target, the mean datastore queries
// per check OpenFGA performs, and records it on corpus.DSQueries. For each
// target it replays a small distinct batch of that target's corpus checks —
// one relation at a time, at HIGHER_CONSISTENCY so they bypass the check cache
// and hit the datastore — and diffs the server's openfga_datastore_query_count
// histogram around the batch. It is best-effort: a failed snapshot or a target
// with no histogram movement is left unattributed, never fatal. It runs after
// the corpus is built and does not consume the generator RNG, so the corpus
// entries stay deterministic; only the (measured) DSQueries values vary.
func attributeDatastoreQueries(client *FGAClient, scraper *MetricsScraper, storeID, modelID string, corpus *Corpus) {
	byTarget := map[string][]CorpusEntry{}
	seen := map[string]map[string]bool{}
	for _, e := range corpus.Entries {
		if len(byTarget[e.Target]) >= attributeBatchSize {
			continue
		}
		if seen[e.Target] == nil {
			seen[e.Target] = map[string]bool{}
		}
		k := e.key()
		if seen[e.Target][k] {
			continue // distinct checks only: duplicates add no information
		}
		seen[e.Target][k] = true
		byTarget[e.Target] = append(byTarget[e.Target], e)
	}
	out := map[string]float64{}
	for _, target := range sortedKeys(byTarget) {
		batch := byTarget[target]
		before, err := scraper.Snapshot()
		if err != nil {
			fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("probe: DS attribution skipped for %s: %v", target, err)))
			continue
		}
		runCheckBatch(client, storeID, modelID, batch)
		after, err := scraper.Snapshot()
		if err != nil {
			fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("probe: DS attribution skipped for %s: %v", target, err)))
			continue
		}
		sum, count := dsQueryDiff(before, after)
		if count <= 0 {
			continue // no recorded checks in the window; leave unattributed
		}
		out[target] = sum / count
	}
	if len(out) > 0 {
		corpus.DSQueries = out
	}
}

// runCheckBatch fires each entry once at HIGHER_CONSISTENCY, at low
// concurrency, discarding outcomes and errors. The DS-attribution pass reads
// the effect on the server's datastore-query histogram, not the check results.
func runCheckBatch(client *FGAClient, storeID, modelID string, batch []CorpusEntry) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, attributeConcurrency)
	for i := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(e CorpusEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			client.Check(storeID, CheckRequest{
				TupleKey: CheckTupleKey{
					User:     e.User,
					Relation: e.Relation,
					Object:   e.Object,
				},
				ContextualTuples:     contextualTupleKeys(e.ContextualTuples),
				Context:              e.Context,
				AuthorizationModelID: modelID,
				Consistency:          "HIGHER_CONSISTENCY",
			})
		}(batch[i])
	}
	wg.Wait()
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
			return nil, fmt.Errorf("contextual relation %q does not accept direct tuples; use the direct relation that carries the request-local fact, not the computed relation that depends on it", key)
		}
		if !hasPlainDirectRef(refs) {
			return nil, fmt.Errorf("contextual relation %q does not accept plain direct user tuples", key)
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
// ratio keeps the natural mix. maxDup bounds how far the scarcer class may be
// duplicated: when hitting the ratio would replicate entries more than maxDup
// times on average, the target keeps its natural mix instead (replaying a
// handful of checks thousands of times measures the server's cache, not the
// model). A non-positive maxDup means unbounded.
func resample(entries []CorpusEntry, ratio, maxDup float64, rng *rand.Rand) []CorpusEntry {
	if ratio < 0 {
		return entries
	}
	byTarget := map[string][]CorpusEntry{}
	for _, e := range entries {
		byTarget[e.Target] = append(byTarget[e.Target], e)
	}
	// Iterate targets in sorted order: map iteration order would consume the
	// RNG differently on every run, breaking corpus reproducibility for a
	// fixed random_seed.
	targets := make([]string, 0, len(byTarget))
	for t := range byTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	var out []CorpusEntry
	for _, target := range targets {
		group := byTarget[target]
		var allowed, denied []CorpusEntry
		for _, e := range group {
			if e.Expected {
				allowed = append(allowed, e)
			} else {
				denied = append(denied, e)
			}
		}
		if len(allowed) == 0 || len(denied) == 0 {
			fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("probe: target %s has only %s outcomes (allowed=%d denied=%d); keeping natural mix",
				target, map[bool]string{true: "allowed", false: "denied"}[len(allowed) > 0], len(allowed), len(denied))))
			out = append(out, group...)
			continue
		}
		n := len(group)
		wantAllowed := int(float64(n) * ratio)
		wantDenied := n - wantAllowed
		if maxDup > 0 && (float64(wantAllowed) > maxDup*float64(len(allowed)) || float64(wantDenied) > maxDup*float64(len(denied))) {
			fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("probe: target %s would need >%.0fx duplication to reach allowed_ratio %.2f (natural mix: allowed=%d denied=%d); keeping natural mix",
				target, maxDup, ratio, len(allowed), len(denied))))
			out = append(out, group...)
			continue
		}
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
