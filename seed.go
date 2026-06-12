package main

// seed.go generates a tuple graph directly from the model. Instances of every
// type are partitioned into cohorts (think: tenants). Tuples link instances
// within the same cohort, which is what makes deep paths and intersection
// relations actually resolve to allowed for some subjects instead of the
// graph being random noise. Conditioned direct types get condition context
// generated from shared value pools so that CEL expressions evaluate true at
// a controllable rate.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

type World struct {
	Instances map[string][]string // type -> instance ids ("user:user-c0-001")
	Cohort    map[string]int      // instance id -> cohort
	cfg       *Config
	analysis  *Analysis
	rng       *rand.Rand
	pools     map[string][]string
}

func NewWorld(a *Analysis, cfg *Config) *World {
	seed := cfg.RandomSeed
	if seed == 0 {
		seed = 42
	}
	w := &World{
		Instances: map[string][]string{},
		Cohort:    map[string]int{},
		cfg:       cfg,
		analysis:  a,
		rng:       rand.New(rand.NewSource(seed)),
		pools:     map[string][]string{},
	}
	for name, p := range cfg.Pools {
		w.pools[name] = p.Materialize()
	}
	for _, t := range a.Types {
		n := cfg.Seed.DefaultCount
		if v, ok := cfg.Seed.Instances[t]; ok {
			n = v
		}
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			cohort := i % cfg.Seed.Cohorts
			id := fmt.Sprintf("%s:%s-c%d-%03d", t, t, cohort, i)
			ids[i] = id
			w.Cohort[id] = cohort
		}
		w.Instances[t] = ids
	}
	return w
}

func (w *World) pool(name string) []string {
	if p, ok := w.pools[name]; ok {
		return p
	}
	return w.pools["default"]
}

// pickInCohort returns a random instance of typ in the given cohort, falling
// back to any instance when the cohort has none.
func (w *World) pickInCohort(typ string, cohort int) string {
	ids := w.Instances[typ]
	if len(ids) == 0 {
		return ""
	}
	// Instances are laid out round-robin by cohort, so members of cohort c
	// sit at indices c, c+K, c+2K, ...
	k := w.cfg.Seed.Cohorts
	count := (len(ids) - 1 - cohort) / k
	if cohort < len(ids) {
		idx := cohort + k*w.rng.Intn(count+1)
		return ids[idx]
	}
	return ids[w.rng.Intn(len(ids))]
}

// GenerateTuples walks every assignable (type, relation, user type) and emits
// tuples. Self-referential links (e.g. folder#parent pointing at folder)
// only point from higher to lower instance indices, guaranteeing a DAG.
func (w *World) GenerateTuples() []TupleKey {
	var tuples []TupleKey
	seen := map[string]bool{}
	a := w.analysis
	add := func(t TupleKey) {
		key := t.User + "|" + t.Relation + "|" + t.Object
		if seen[key] {
			return
		}
		seen[key] = true
		tuples = append(tuples, t)
	}

	types := append([]string{}, a.Types...)
	sort.Strings(types)
	for _, typ := range types {
		rels := a.DirectRefs[typ]
		relNames := make([]string, 0, len(rels))
		for r := range rels {
			relNames = append(relNames, r)
		}
		sort.Strings(relNames)
		for _, rel := range relNames {
			fanout := w.cfg.Seed.DefaultFanout
			if v, ok := w.cfg.Seed.Fanout[typ+"#"+rel]; ok {
				fanout = v
			}
			for _, obj := range w.Instances[typ] {
				cohort := w.Cohort[obj]
				objIdx := instanceIndex(obj)
				for _, ref := range rels[rel] {
					if ref.Wildcard != nil {
						if w.rng.Float64() <= w.cfg.Seed.WildcardProb {
							add(TupleKey{
								User:      ref.Type + ":*",
								Relation:  rel,
								Object:    obj,
								Condition: w.tupleCondition(ref.Condition),
							})
						}
						continue
					}
					for i := 0; i < fanout; i++ {
						var subject string
						if ref.Type == typ {
							subject = w.pickLowerIndex(ref.Type, cohort, objIdx)
							if subject == "" {
								break
							}
						} else {
							subject = w.pickInCohort(ref.Type, cohort)
						}
						if subject == "" {
							break
						}
						if ref.Relation != "" {
							subject = subject + "#" + ref.Relation
						}
						add(TupleKey{
							User:      subject,
							Relation:  rel,
							Object:    obj,
							Condition: w.tupleCondition(ref.Condition),
						})
					}
				}
			}
		}
	}
	return tuples
}

func instanceIndex(id string) int {
	var idx int
	fmt.Sscanf(id[strings.LastIndex(id, "-")+1:], "%d", &idx)
	return idx
}

func (w *World) pickLowerIndex(typ string, cohort, below int) string {
	k := w.cfg.Seed.Cohorts
	var candidates []string
	for i := cohort; i < below; i += k {
		if i < len(w.Instances[typ]) {
			candidates = append(candidates, w.Instances[typ][i])
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[w.rng.Intn(len(candidates))]
}

// tupleCondition builds the write-time context for a conditioned tuple.
func (w *World) tupleCondition(condName string) *TupleCondition {
	if condName == "" {
		return nil
	}
	tupleSide, _ := w.analysis.TupleContextParams(condName, w.cfg)
	ctx := map[string]any{}
	cond := w.analysis.Model.Conditions[condName]
	for _, p := range tupleSide {
		ctx[p] = w.genValue(condName, p, cond.Parameters[p])
	}
	return &TupleCondition{Name: condName, Context: ctx}
}

// RequestContext builds check-time context covering the request-side
// parameters of every condition in the model. Unused keys are ignored by the
// server, so a single merged context is safe for any check.
func (w *World) RequestContext(rng *rand.Rand) map[string]any {
	ctx := map[string]any{}
	names := make([]string, 0, len(w.analysis.Model.Conditions))
	for n := range w.analysis.Model.Conditions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, condName := range names {
		_, reqSide := w.analysis.TupleContextParams(condName, w.cfg)
		cond := w.analysis.Model.Conditions[condName]
		for _, p := range reqSide {
			if _, exists := ctx[p]; !exists {
				ctx[p] = w.genValueWith(rng, condName, p, cond.Parameters[p])
			}
		}
	}
	return ctx
}

func (w *World) genValue(condName, param string, t ParamTypeRef) any {
	return w.genValueWith(w.rng, condName, param, t)
}

func (w *World) genValueWith(rng *rand.Rand, condName, param string, t ParamTypeRef) any {
	poolName := "default"
	keys := 4
	if cc, ok := w.cfg.Conditions[condName]; ok {
		if pc, ok := cc.ParamConfigs[param]; ok {
			if pc.Pool != "" {
				poolName = pc.Pool
			}
			if pc.Keys > 0 {
				keys = pc.Keys
			}
		}
	}
	pool := w.pool(poolName)
	pick := func() string { return pool[rng.Intn(len(pool))] }
	switch t.TypeName {
	case "TYPE_NAME_STRING":
		return pick()
	case "TYPE_NAME_INT", "TYPE_NAME_UINT":
		return rng.Intn(100)
	case "TYPE_NAME_DOUBLE":
		return rng.Float64() * 100
	case "TYPE_NAME_BOOL":
		return rng.Intn(2) == 0
	case "TYPE_NAME_TIMESTAMP":
		return time.Now().UTC().Format(time.RFC3339)
	case "TYPE_NAME_DURATION":
		return "60s"
	case "TYPE_NAME_LIST":
		out := make([]any, 0, keys)
		for i := 0; i < keys; i++ {
			out = append(out, pick())
		}
		return out
	case "TYPE_NAME_MAP":
		out := map[string]any{}
		for len(out) < keys && len(out) < len(pool) {
			out[pick()] = "granted"
		}
		return out
	default:
		return pick()
	}
}

// SeedStore writes tuples with parallel workers and returns write throughput.
func SeedStore(client *FGAClient, storeID, modelID string, tuples []TupleKey, cfg *Config) (time.Duration, error) {
	batches := make(chan []TupleKey, cfg.Seed.Writers*2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	start := time.Now()
	for i := 0; i < cfg.Seed.Writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batches {
				if err := client.WriteTuples(storeID, modelID, batch); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < len(tuples); i += cfg.Seed.BatchSize {
		end := i + cfg.Seed.BatchSize
		if end > len(tuples) {
			end = len(tuples)
		}
		batches <- tuples[i:end]
	}
	close(batches)
	wg.Wait()
	return time.Since(start), firstErr
}
