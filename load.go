package main

// load.go drives the measured phase. Workers replay corpus entries either in
// a closed loop (back-to-back, bounded by concurrency) or at a fixed offered
// rate via a shared ticker. Every sample carries its target relation and the
// conditioned/unconditioned tag so CEL overhead can be reported separately.
// Warmup samples are discarded.

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Sample struct {
	Target      string
	Conditioned bool
	Latency     time.Duration
	Err         bool
	Mismatch    bool
	Items       int // 1 for check; batch size for batch-check
}

type LoadResult struct {
	Endpoint    string
	Consistency string
	Concurrency int
	OfferedRate int
	Warmup      time.Duration
	Duration    time.Duration
	WallClock   time.Duration
	Samples     []Sample
	TotalErrors int64
	TotalChecks int64
	Mismatches  int64
}

func RunLoad(client *FGAClient, corpus *Corpus, cfg *Config) (*LoadResult, error) {
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

	var rateCh chan struct{}
	stopRate := make(chan struct{})
	if lc.Rate > 0 {
		rateCh = make(chan struct{}, lc.Rate)
		interval := time.Second / time.Duration(lc.Rate)
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					select {
					case rateCh <- struct{}{}:
					default: // queue full; drop the slot rather than build backlog
					}
				case <-stopRate:
					return
				}
			}
		}()
	}

	start := time.Now()
	warmupEnd := start.Add(lc.Warmup)
	deadline := start.Add(lc.Warmup + lc.Duration)

	var wg sync.WaitGroup
	sampleCh := make(chan Sample, 4096)
	var errors, checks, mismatches int64

	worker := func(id int) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(cfg.RandomSeed + int64(id)*7919))
		for time.Now().Before(deadline) {
			if rateCh != nil {
				select {
				case <-rateCh:
				case <-time.After(50 * time.Millisecond):
					continue
				}
			}
			var s Sample
			if lc.Endpoint == "batch-check" {
				s = doBatch(client, corpus, cfg, rng)
			} else {
				s = doCheck(client, corpus, cfg, rng)
			}
			atomic.AddInt64(&checks, int64(s.Items))
			if s.Err {
				atomic.AddInt64(&errors, 1)
			}
			if s.Mismatch {
				atomic.AddInt64(&mismatches, 1)
			}
			if time.Now().After(warmupEnd) {
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
	for s := range sampleCh {
		res.Samples = append(res.Samples, s)
	}
	<-done
	res.WallClock = time.Since(start)
	res.TotalErrors = errors
	res.TotalChecks = checks
	res.Mismatches = mismatches
	return res, nil
}

func doCheck(client *FGAClient, corpus *Corpus, cfg *Config, rng *rand.Rand) Sample {
	e := corpus.Entries[rng.Intn(len(corpus.Entries))]
	t0 := time.Now()
	allowed, err := client.Check(corpus.StoreID, CheckRequest{
		TupleKey:             CheckTupleKey{User: e.User, Relation: e.Relation, Object: e.Object},
		Context:              e.Context,
		AuthorizationModelID: corpus.ModelID,
		Consistency:          cfg.Load.Consistency,
	})
	s := Sample{Target: e.Target, Conditioned: e.Conditioned, Latency: time.Since(t0), Items: 1}
	if err != nil {
		s.Err = true
	} else if cfg.Load.VerifyResults && allowed != e.Expected {
		s.Mismatch = true
	}
	return s
}

func doBatch(client *FGAClient, corpus *Corpus, cfg *Config, rng *rand.Rand) Sample {
	n := cfg.Load.BatchSize
	items := make([]BatchCheckItem, n)
	conditioned := false
	expected := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		e := corpus.Entries[rng.Intn(len(corpus.Entries))]
		id := fmt.Sprintf("c%d", i)
		items[i] = BatchCheckItem{
			TupleKey:      CheckTupleKey{User: e.User, Relation: e.Relation, Object: e.Object},
			Context:       e.Context,
			CorrelationID: id,
		}
		expected[id] = e.Expected
		conditioned = conditioned || e.Conditioned
	}
	t0 := time.Now()
	resp, err := client.BatchCheck(corpus.StoreID, BatchCheckRequest{
		Checks:               items,
		AuthorizationModelID: corpus.ModelID,
		Consistency:          cfg.Load.Consistency,
	})
	s := Sample{Target: "batch", Conditioned: conditioned, Latency: time.Since(t0), Items: n}
	if err != nil {
		s.Err = true
		return s
	}
	if cfg.Load.VerifyResults {
		for id, r := range resp.Result {
			if want, ok := expected[id]; ok && r.Error == nil && r.Allowed != want {
				s.Mismatch = true
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
		lats = append(lats, s.Latency)
		sum += s.Latency
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
