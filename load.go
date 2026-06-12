package main

// load.go drives the measured phase. Workers replay corpus entries either in
// a closed loop (back-to-back, bounded by concurrency) or at a fixed offered
// rate via a shared ticker. Every sample carries its target relation and the
// conditioned/unconditioned tag so CEL overhead can be reported separately.
// Warmup samples are discarded.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Sample struct {
	Target      string
	Conditioned bool
	Contextual  bool
	Latency     time.Duration // service latency: request start -> response
	RespLatency time.Duration // fixed-rate only: intended send time -> response (includes queueing delay)
	Completed   time.Time
	Err         bool
	ErrClass    string // timeout | connection | 4xx | 5xx | decode
	ErrMsg      string
	Mismatch    bool
	Items       int // 1 for check; batch size for batch-check
}

type LoadResult struct {
	Endpoint       string
	Consistency    string
	Concurrency    int
	OfferedRate    int
	Warmup         time.Duration
	Duration       time.Duration
	WallClock      time.Duration
	MeasuredWindow time.Duration // first to last measured-sample completion
	DroppedSlots   int64         // fixed-rate slots dropped because workers fell a full buffer behind
	Samples        []Sample
	TotalErrors    int64
	TotalChecks    int64
	Mismatches     int64
	ErrorsByClass  map[string]int64
	ErrorSamples    []string       // first few verbatim error strings from the measured phase
	Server          *ServerMetrics // diffed Prometheus view of the measured phase; nil when not scraped
	MismatchRecords []MismatchRecord
}

const (
	maxErrorSamples    = 5
	maxMismatchRecords = 100
)

// MismatchRecord identifies a corpus entry whose response under load differed
// from its probe-time expectation — the raw material for investigating cache
// staleness or consistency effects.
type MismatchRecord struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
	Target   string `json:"target"`
	Expected bool   `json:"expected"`
	Observed bool   `json:"observed"`
}

// mismatchRecorder collects deduplicated mismatches, capped so a
// systematically stale cache can't balloon memory. The lock is only taken on
// mismatches, which are exceptional.
type mismatchRecorder struct {
	mu   sync.Mutex
	seen map[string]bool
	out  []MismatchRecord
}

func newMismatchRecorder() *mismatchRecorder {
	return &mismatchRecorder{seen: map[string]bool{}}
}

func (r *mismatchRecorder) record(e *CorpusEntry, observed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := e.key()
	if r.seen[k] || len(r.out) >= maxMismatchRecords {
		return
	}
	r.seen[k] = true
	r.out = append(r.out, MismatchRecord{
		User: e.User, Relation: e.Relation, Object: e.Object,
		Target: e.Target, Expected: e.Expected, Observed: observed,
	})
}

// corpusPicker selects corpus entries for replay. Without configured weights
// it is uniform over entries (every corpus entry equally likely, the original
// behavior). With weights, a target is chosen proportionally to its weight
// and an entry uniformly within it, so the traffic mix follows the configured
// production-like skew.
type corpusPicker struct {
	entries []CorpusEntry
	groups  [][]int   // entry indices per weighted target
	cum     []float64 // cumulative weights aligned with groups
	total   float64
}

func newCorpusPicker(c *Corpus) *corpusPicker {
	p := &corpusPicker{entries: c.Entries}
	if len(c.Weights) == 0 {
		return p
	}
	byTarget := map[string][]int{}
	var targets []string
	for i, e := range c.Entries {
		if _, ok := byTarget[e.Target]; !ok {
			targets = append(targets, e.Target)
		}
		byTarget[e.Target] = append(byTarget[e.Target], i)
	}
	sort.Strings(targets)
	for _, t := range targets {
		w := c.Weights[t]
		if w <= 0 {
			w = 1
		}
		p.total += w
		p.groups = append(p.groups, byTarget[t])
		p.cum = append(p.cum, p.total)
	}
	return p
}

func (p *corpusPicker) pick(rng *rand.Rand) *CorpusEntry {
	if len(p.groups) == 0 {
		return &p.entries[rng.Intn(len(p.entries))]
	}
	x := rng.Float64() * p.total
	i := sort.SearchFloat64s(p.cum, x)
	if i >= len(p.groups) {
		i = len(p.groups) - 1
	}
	g := p.groups[i]
	return &p.entries[g[rng.Intn(len(g))]]
}

// classifyErr buckets a request error for reporting. The classes are coarse on
// purpose; ErrorSamples carries the verbatim strings for diagnosis.
func classifyErr(err error) string {
	var he *HTTPError
	if errors.As(err, &he) {
		if he.StatusCode >= 500 {
			return "5xx"
		}
		return "4xx"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var se *json.SyntaxError
	var ute *json.UnmarshalTypeError
	if errors.As(err, &se) || errors.As(err, &ute) {
		return "decode"
	}
	return "connection"
}

// RunLoad replays the corpus. scraper may be nil; when set, server metrics
// are snapshotted at the warmup/measured boundary and after the last worker
// exits, off the request path.
func RunLoad(client *FGAClient, corpus *Corpus, cfg *Config, scraper *MetricsScraper) (*LoadResult, error) {
	if len(corpus.Entries) == 0 {
		return nil, fmt.Errorf("corpus is empty; run probe first")
	}
	lc := cfg.Load
	res := &LoadResult{
		Endpoint:    lc.Endpoint,
		Consistency: lc.Consistency,
		Concurrency: lc.Concurrency,
		OfferedRate: lc.Rate,
		Warmup:      lc.Warmup,
		Duration:    lc.Duration,
	}

	start := time.Now()
	warmupEnd := start.Add(lc.Warmup)
	deadline := start.Add(lc.Warmup + lc.Duration)

	beforeSnap := make(chan *snapshot, 1)
	if scraper != nil {
		go func() {
			time.Sleep(time.Until(warmupEnd))
			s, err := scraper.Snapshot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "metrics: snapshot at measured-phase start failed, skipping server-side view: %v\n", err)
				return
			}
			beforeSnap <- s
		}()
	}

	// Fixed-rate mode dispatches *intended* send times computed from the slot
	// schedule (slot N fires at start + N*interval), never from when a worker
	// happened to be free. Workers measure response latency against that
	// intended time, so queueing delay under saturation shows up in the numbers
	// instead of being coordinated away. The buffer holds one second of slots;
	// only when workers fall a full second behind do we drop (and count) slots.
	var rateCh chan time.Time
	stopRate := make(chan struct{})
	var droppedSlots int64
	if lc.Rate > 0 {
		rateCh = make(chan time.Time, lc.Rate)
		interval := time.Second / time.Duration(lc.Rate)
		go func() {
			var timer *time.Timer
			for n := int64(0); ; n++ {
				intended := start.Add(time.Duration(n) * interval)
				if d := time.Until(intended); d > 0 {
					if timer == nil {
						timer = time.NewTimer(d)
					} else {
						timer.Reset(d)
					}
					select {
					case <-timer.C:
					case <-stopRate:
						return
					}
				}
				select {
				case <-stopRate:
					return
				case rateCh <- intended:
				default:
					atomic.AddInt64(&droppedSlots, 1)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	sampleCh := make(chan Sample, 4096)
	var errCount, checks, mismatches int64
	picker := newCorpusPicker(corpus)
	mism := newMismatchRecorder()

	worker := func(id int) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(cfg.RandomSeed + int64(id)*7919))
		for time.Now().Before(deadline) {
			var intended time.Time
			if rateCh != nil {
				select {
				case intended = <-rateCh:
				case <-time.After(50 * time.Millisecond):
					continue
				}
			}
			var s Sample
			if lc.Endpoint == "batch-check" {
				s = doBatch(client, picker, mism, corpus, cfg, rng)
			} else {
				s = doCheck(client, picker, mism, corpus, cfg, rng)
			}
			if !intended.IsZero() {
				s.RespLatency = s.Completed.Sub(intended)
			}
			atomic.AddInt64(&checks, int64(s.Items))
			if s.Err {
				atomic.AddInt64(&errCount, 1)
			}
			if s.Mismatch {
				atomic.AddInt64(&mismatches, 1)
			}
			if s.Completed.After(warmupEnd) {
				sampleCh <- s
			}
		}
	}

	wg.Add(lc.Concurrency)
	for i := 0; i < lc.Concurrency; i++ {
		go worker(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(sampleCh)
		close(stopRate)
		close(done)
	}()
	res.ErrorsByClass = map[string]int64{}
	var firstDone, lastDone time.Time
	for s := range sampleCh {
		if firstDone.IsZero() || s.Completed.Before(firstDone) {
			firstDone = s.Completed
		}
		if s.Completed.After(lastDone) {
			lastDone = s.Completed
		}
		if s.Err {
			res.ErrorsByClass[s.ErrClass]++
			if len(res.ErrorSamples) < maxErrorSamples && s.ErrMsg != "" {
				res.ErrorSamples = append(res.ErrorSamples, s.ErrMsg)
			}
		}
		res.Samples = append(res.Samples, s)
	}
	<-done
	if scraper != nil {
		if after, err := scraper.Snapshot(); err != nil {
			fmt.Fprintf(os.Stderr, "metrics: snapshot at measured-phase end failed, skipping server-side view: %v\n", err)
		} else {
			select {
			case before := <-beforeSnap:
				res.Server = buildServerMetrics(before, after)
			default: // start snapshot failed; nothing to diff against
			}
		}
	}
	res.WallClock = time.Since(start)
	res.MeasuredWindow = lastDone.Sub(firstDone)
	res.DroppedSlots = atomic.LoadInt64(&droppedSlots)
	res.TotalErrors = errCount
	res.TotalChecks = checks
	res.Mismatches = mismatches
	res.MismatchRecords = mism.out
	return res, nil
}

// RunSweep steps through the configured offered rates against the same corpus
// and store. Warmup runs once, before the first step; later steps inherit a
// warm server and connection pool.
func RunSweep(client *FGAClient, corpus *Corpus, cfg *Config, scraper *MetricsScraper) ([]*LoadResult, error) {
	sw := cfg.Load.Sweep
	results := make([]*LoadResult, 0, len(sw.Rates))
	for i, rate := range sw.Rates {
		stepCfg := *cfg
		stepCfg.Load.Rate = rate
		stepCfg.Load.Duration = sw.StepDuration
		if i > 0 {
			stepCfg.Load.Warmup = 0
		}
		fmt.Printf("sweep step %d/%d: offered %d req/s for %s\n", i+1, len(sw.Rates), rate, sw.StepDuration)
		res, err := RunLoad(client, corpus, &stepCfg, scraper)
		if err != nil {
			return nil, fmt.Errorf("sweep step %d (rate %d): %w", i+1, rate, err)
		}
		st := Summarize(res.Samples)
		achieved := 0.0
		if res.MeasuredWindow > 0 {
			achieved = float64(len(res.Samples)) / res.MeasuredWindow.Seconds()
		}
		fmt.Printf("  achieved %.0f req/s | p50 %s p99 %s | errors %d | %d slots dropped\n",
			achieved, ms(st.P50), ms(st.P99), st.Errors, res.DroppedSlots)
		results = append(results, res)
	}
	return results, nil
}

func doCheck(client *FGAClient, picker *corpusPicker, mism *mismatchRecorder, corpus *Corpus, cfg *Config, rng *rand.Rand) Sample {
	e := picker.pick(rng)
	t0 := time.Now()
	allowed, err := client.Check(corpus.StoreID, CheckRequest{
		TupleKey:             CheckTupleKey{User: e.User, Relation: e.Relation, Object: e.Object},
		ContextualTuples:     contextualTupleKeys(e.ContextualTuples),
		Context:              e.Context,
		AuthorizationModelID: corpus.ModelID,
		Consistency:          cfg.Load.Consistency,
	})
	completed := time.Now()
	s := Sample{Target: e.Target, Conditioned: e.Conditioned, Contextual: e.Contextual, Latency: completed.Sub(t0), Completed: completed, Items: 1}
	if err != nil {
		s.Err = true
		s.ErrClass = classifyErr(err)
		s.ErrMsg = err.Error()
	} else if cfg.Load.VerifyResults && allowed != e.Expected {
		s.Mismatch = true
		mism.record(e, allowed)
	}
	return s
}

func doBatch(client *FGAClient, picker *corpusPicker, mism *mismatchRecorder, corpus *Corpus, cfg *Config, rng *rand.Rand) Sample {
	n := cfg.Load.BatchSize
	items := make([]BatchCheckItem, n)
	conditioned := false
	contextual := false
	entries := make(map[string]*CorpusEntry, n)
	for i := 0; i < n; i++ {
		e := picker.pick(rng)
		id := fmt.Sprintf("c%d", i)
		items[i] = BatchCheckItem{
			TupleKey:         CheckTupleKey{User: e.User, Relation: e.Relation, Object: e.Object},
			ContextualTuples: contextualTupleKeys(e.ContextualTuples),
			Context:          e.Context,
			CorrelationID:    id,
		}
		entries[id] = e
		conditioned = conditioned || e.Conditioned
		contextual = contextual || e.Contextual
	}
	t0 := time.Now()
	resp, err := client.BatchCheck(corpus.StoreID, BatchCheckRequest{
		Checks:               items,
		AuthorizationModelID: corpus.ModelID,
		Consistency:          cfg.Load.Consistency,
	})
	completed := time.Now()
	s := Sample{Target: "batch", Conditioned: conditioned, Contextual: contextual, Latency: completed.Sub(t0), Completed: completed, Items: n}
	if err != nil {
		s.Err = true
		s.ErrClass = classifyErr(err)
		s.ErrMsg = err.Error()
		return s
	}
	if cfg.Load.VerifyResults {
		for id, r := range resp.Result {
			if e, ok := entries[id]; ok && r.Error == nil && r.Allowed != e.Expected {
				s.Mismatch = true
				mism.record(e, r.Allowed)
			}
		}
	}
	return s
}

// Stats summarizes a set of latency samples.
type Stats struct {
	Count  int           `json:"count"`
	Errors int           `json:"errors"`
	Items  int           `json:"items"`
	Min    time.Duration `json:"min_ns"`
	Mean   time.Duration `json:"mean_ns"`
	P50    time.Duration `json:"p50_ns"`
	P90    time.Duration `json:"p90_ns"`
	P95    time.Duration `json:"p95_ns"`
	P99    time.Duration `json:"p99_ns"`
	Max    time.Duration `json:"max_ns"`
}

func Summarize(samples []Sample) Stats {
	return summarizeBy(samples, func(s Sample) time.Duration { return s.Latency })
}

// SummarizeResponse summarizes response latency (intended send time to
// completion); only meaningful for fixed-rate runs.
func SummarizeResponse(samples []Sample) Stats {
	return summarizeBy(samples, func(s Sample) time.Duration { return s.RespLatency })
}

func summarizeBy(samples []Sample, latency func(Sample) time.Duration) Stats {
	st := Stats{}
	if len(samples) == 0 {
		return st
	}
	lats := make([]time.Duration, 0, len(samples))
	var sum time.Duration
	for _, s := range samples {
		st.Items += s.Items
		if s.Err {
			st.Errors++
			continue
		}
		l := latency(s)
		lats = append(lats, l)
		sum += l
	}
	st.Count = len(lats)
	if st.Count == 0 {
		return st
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) time.Duration {
		idx := int(p*float64(len(lats))) - 1
		if idx < 0 {
			idx = 0
		}
		return lats[idx]
	}
	st.Min = lats[0]
	st.Max = lats[len(lats)-1]
	st.Mean = sum / time.Duration(st.Count)
	st.P50, st.P90, st.P95, st.P99 = pct(0.50), pct(0.90), pct(0.95), pct(0.99)
	return st
}
