package main

import (
	"math"
	"sort"
	"testing"
	"time"
)

func TestLatencyDigestMatchesExactWithinTolerance(t *testing.T) {
	const n = 50000
	samples := make([]Sample, 0, n)
	var merged latencyDigest
	var even latencyDigest
	var odd latencyDigest
	for i := 0; i < n; i++ {
		ns := int64(750 + (i*i*7919)%50_000_000 + i*37)
		latency := time.Duration(ns)
		samples = append(samples, Sample{Latency: latency, Items: 1})
		if i%2 == 0 {
			even.Add(latency)
		} else {
			odd.Add(latency)
		}
	}
	merged.Merge(even)
	merged.Merge(odd)

	got := Summarize(samples)
	want := exactStats(samples, func(s Sample) time.Duration { return s.Latency })
	for name, pair := range map[string][2]time.Duration{
		"p50": {got.P50, want.P50},
		"p90": {got.P90, want.P90},
		"p95": {got.P95, want.P95},
		"p99": {got.P99, want.P99},
	} {
		if relDurationDiff(pair[0], pair[1]) > 0.01 {
			t.Fatalf("%s = %v, exact %v (relative diff %.4f)", name, pair[0], pair[1], relDurationDiff(pair[0], pair[1]))
		}
	}

	mergedStats := merged.Stats()
	if mergedStats.P99 != got.P99 || mergedStats.Count != got.Count {
		t.Fatalf("merged digest stats = count %d p99 %v, single-pass count %d p99 %v", mergedStats.Count, mergedStats.P99, got.Count, got.P99)
	}
	if got.Min != want.Min || got.Max != want.Max || got.Count != want.Count {
		t.Fatalf("min/max/count drifted: got %+v exact %+v", got, want)
	}
}

func exactStats(samples []Sample, latency func(Sample) time.Duration) Stats {
	var st Stats
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

func relDurationDiff(got, want time.Duration) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(float64(got-want)) / float64(want)
}
