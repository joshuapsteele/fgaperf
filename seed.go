package main

// seed.go generates a tuple graph directly from the model. Instances of every
// type are partitioned into cohorts (think: tenants). Tuples link instances
// within the same cohort, which is what makes deep paths and intersection
// relations actually resolve to allowed for some subjects instead of the
// graph being random noise. Conditioned direct types get condition context
// generated from shared value pools so that CEL expressions evaluate true at
// a controllable rate.

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	contextual := contextualSet(w.cfg)
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
			if contextual[typ+"#"+rel] {
				continue
			}
			relFanout := w.cfg.Seed.DefaultFanout
			if v, ok := w.cfg.Seed.Fanout[typ+"#"+rel]; ok {
				relFanout = v
			}
			wildcardProb := w.cfg.Seed.WildcardProb
			if v, ok := w.cfg.Seed.WildcardProbs[typ+"#"+rel]; ok {
				wildcardProb = v
			}
			for _, obj := range w.Instances[typ] {
				cohort := w.Cohort[obj]
				objIdx := instanceIndex(obj)
				for _, ref := range rels[rel] {
					if ref.Wildcard != nil {
						if w.rng.Float64() <= wildcardProb {
							add(TupleKey{
								User:      ref.Type + ":*",
								Relation:  rel,
								Object:    obj,
								Condition: w.tupleCondition(ref.Condition),
							})
						}
						continue
					}
					// Per-user-type override: "type#relation@usertype" beats
					// the bare "type#relation" key for this ref only.
					fanout := relFanout
					suffix := ref.Type
					if ref.Relation != "" {
						suffix += "#" + ref.Relation
					}
					if v, ok := w.cfg.Seed.Fanout[typ+"#"+rel+"@"+suffix]; ok {
						fanout = v
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

func contextualSet(cfg *Config) map[string]bool {
	out := map[string]bool{}
	for _, rel := range cfg.Contextual.Relations {
		out[rel] = true
	}
	return out
}

type TupleIndex struct {
	byObjectAndUserType map[string][]TupleKey
}

func NewTupleIndex(tuples []TupleKey) *TupleIndex {
	idx := &TupleIndex{byObjectAndUserType: map[string][]TupleKey{}}
	for _, t := range tuples {
		userType := typeOfUser(t.User)
		if userType == "" {
			continue
		}
		key := t.Object + "|" + userType
		idx.byObjectAndUserType[key] = append(idx.byObjectAndUserType[key], t)
	}
	return idx
}

func (idx *TupleIndex) RelatedObjects(object, objectType string) []string {
	if idx == nil {
		return nil
	}
	tuples := idx.byObjectAndUserType[object+"|"+objectType]
	out := make([]string, 0, len(tuples))
	seen := map[string]bool{}
	for _, t := range tuples {
		user := strings.SplitN(t.User, "#", 2)[0]
		if !seen[user] {
			seen[user] = true
			out = append(out, user)
		}
	}
	return out
}

func typeOfUser(user string) string {
	user = strings.SplitN(user, "#", 2)[0]
	if strings.HasSuffix(user, ":*") {
		return strings.TrimSuffix(user, ":*")
	}
	i := strings.Index(user, ":")
	if i < 0 {
		return ""
	}
	return user[:i]
}

func typeOfObject(object string) string {
	i := strings.Index(object, ":")
	if i < 0 {
		return ""
	}
	return object[:i]
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
	var dist *KeysDistribution
	if cc, ok := w.cfg.Conditions[condName]; ok {
		if pc, ok := cc.ParamConfigs[param]; ok {
			if pc.Pool != "" {
				poolName = pc.Pool
			}
			if pc.Keys > 0 {
				keys = pc.Keys
			}
			dist = pc.KeysDistribution
		}
	}
	pool := w.pool(poolName)
	pick := func() string { return pool[rng.Intn(len(pool))] }
	// keysFor draws per value only when a distribution is configured, so
	// configs without one consume the RNG exactly as before (determinism
	// contract: same seed, same world).
	keysFor := func() int {
		if dist != nil {
			return dist.draw(rng)
		}
		return keys
	}
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
		return seededTimestamp(rng)
	case "TYPE_NAME_DURATION":
		return "60s"
	case "TYPE_NAME_LIST":
		n := keysFor()
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, pick())
		}
		return out
	case "TYPE_NAME_MAP":
		n := keysFor()
		out := map[string]any{}
		for len(out) < n && len(out) < len(pool) {
			out[pick()] = "granted"
		}
		return out
	default:
		return pick()
	}
}

func seededTimestamp(rng *rand.Rand) string {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const secondsInYear = int64(365 * 24 * 60 * 60)
	return base.Add(time.Duration(rng.Int63n(secondsInYear)) * time.Second).Format(time.RFC3339)
}

// batchWatermark tracks the contiguous-from-zero prefix of completed batches.
// Because batch j covers a fixed tuple range, the prefix is always a clean
// tuple prefix — exactly what a resume can skip safely, since generation is
// deterministic. A failed batch leaves a hole that halts the watermark, so the
// checkpoint never claims tuples after the first failure.
type batchWatermark struct {
	seen   []bool
	sizes  []int
	contig int
	prefix int // tuples in the contiguous prefix of completed batches
}

func (w *batchWatermark) complete(idx int) {
	w.seen[idx] = true
	for w.contig < len(w.seen) && w.seen[w.contig] {
		w.prefix += w.sizes[w.contig]
		w.contig++
	}
}

// isDuplicateWriteErr reports whether err is OpenFGA rejecting a write because
// the tuple already exists. On resume this is expected: batches after the last
// clean-prefix batch may have committed before the interruption, and since each
// batch is one atomic transaction aligned identically across runs (generation
// is deterministic), an "already exists" rejection means the whole batch was
// already written and can be skipped.
func isDuplicateWriteErr(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return strings.Contains(strings.ToLower(he.Body), "already exists")
	}
	return false
}

// SeedStore writes tuples[startIndex:] with parallel workers and returns write
// throughput. When checkpoint is non-nil it is called (throttled) with the
// absolute count of tuples in the clean contiguous prefix written so far, so
// `setup` can record a high-water mark for resume. startIndex is 0 for a fresh
// seed; on resume it skips the already-written prefix and (tolerateDup) treats
// already-committed batches past the prefix as done rather than erroring.
func SeedStore(client *FGAClient, storeID, modelID string, tuples []TupleKey, cfg *Config, startIndex int, tolerateDup bool, checkpoint func(written int)) (time.Duration, error) {
	start := time.Now()
	remaining := tuples[startIndex:]

	type job struct {
		idx int
		t   []TupleKey
	}
	type result struct {
		idx int
		n   int
		ok  bool
	}
	var sizes []int
	for i := 0; i < len(remaining); i += cfg.Seed.BatchSize {
		end := i + cfg.Seed.BatchSize
		if end > len(remaining) {
			end = len(remaining)
		}
		sizes = append(sizes, end-i)
	}

	jobs := make(chan job, cfg.Seed.Writers*2)
	results := make(chan result, cfg.Seed.Writers*2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < cfg.Seed.Writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				err := client.WriteTuples(storeID, modelID, j.t)
				if err != nil && tolerateDup && isDuplicateWriteErr(err) {
					err = nil // batch already committed before the interruption
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
				results <- result{idx: j.idx, n: len(j.t), ok: err == nil}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collector: single goroutine owns the watermark and the progress counter.
	var written int64 // total successfully written (for the progress %)
	stopProgress := make(chan struct{})
	progressDone := make(chan struct{})
	if isTerminal(os.Stderr) && len(tuples) > 0 {
		go seedProgress(&written, int64(len(tuples)), int64(startIndex), start, stopProgress, progressDone)
	} else {
		close(progressDone)
	}

	go func() {
		off := 0
		for i, sz := range sizes {
			jobs <- job{idx: i, t: remaining[off : off+sz]}
			off += sz
		}
		close(jobs)
	}()

	wm := &batchWatermark{seen: make([]bool, len(sizes)), sizes: sizes}
	var lastCheckpoint time.Time
	for r := range results {
		if !r.ok {
			continue // hole: watermark halts here so the checkpoint stays a clean prefix
		}
		atomic.AddInt64(&written, int64(r.n))
		wm.complete(r.idx)
		if checkpoint != nil && time.Since(lastCheckpoint) > 2*time.Second {
			checkpoint(startIndex + wm.prefix)
			lastCheckpoint = time.Now()
		}
	}
	if checkpoint != nil {
		checkpoint(startIndex + wm.prefix)
	}
	close(stopProgress)
	<-progressDone
	return time.Since(start), firstErr
}

// seedProgress prints a throttled one-line progress indicator (overwriting via
// carriage return) until stop is closed, then clears the line.
func seedProgress(written *int64, total, startIndex int64, start time.Time, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			fmt.Fprintf(os.Stderr, "\r%-72s\r", "") // clear the line for the final summary
			return
		case <-ticker.C:
			done := startIndex + atomic.LoadInt64(written)
			elapsed := time.Since(start).Seconds()
			rate := 0.0
			if elapsed > 0 {
				rate = float64(atomic.LoadInt64(written)) / elapsed
			}
			eta := "—"
			if rate > 0 && done < total {
				eta = fmtETA(time.Duration(float64(total-done)/rate) * time.Second)
			}
			fmt.Fprintf(os.Stderr, "\rseeding: %d/%d tuples (%.0f%%, %.0f tuples/sec, ETA %s)",
				done, total, 100*float64(done)/float64(total), rate, eta)
		}
	}
}
