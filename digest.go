package main

import (
	"sort"
	"strconv"
	"time"
)

const timelineQuantum = 100 * time.Millisecond

// latencyDigest is a mergeable, bounded-memory latency sketch. Durations are
// grouped into three-significant-digit buckets, so occupied buckets grow with
// the observed value range rather than the sample count.
type latencyDigest struct {
	count   int
	sumNs   float64
	min     time.Duration
	max     time.Duration
	buckets map[int64]int
}

func (d *latencyDigest) Add(v time.Duration) {
	ns := v.Nanoseconds()
	if ns < 0 {
		ns = 0
		v = 0
	}
	if d.buckets == nil {
		d.buckets = map[int64]int{}
	}
	if d.count == 0 {
		d.min = v
		d.max = v
	} else {
		if v < d.min {
			d.min = v
		}
		if v > d.max {
			d.max = v
		}
	}
	d.count++
	d.sumNs += float64(ns)
	d.buckets[latencyBucketLower(ns)]++
}

func (d *latencyDigest) Merge(other latencyDigest) {
	if other.count == 0 {
		return
	}
	if d.buckets == nil {
		d.buckets = map[int64]int{}
	}
	if d.count == 0 {
		d.min = other.min
		d.max = other.max
	} else {
		if other.min < d.min {
			d.min = other.min
		}
		if other.max > d.max {
			d.max = other.max
		}
	}
	d.count += other.count
	d.sumNs += other.sumNs
	for b, n := range other.buckets {
		d.buckets[b] += n
	}
}

func (d latencyDigest) Stats() Stats {
	st := Stats{Count: d.count}
	if d.count == 0 {
		return st
	}
	st.Min = d.min
	st.Max = d.max
	st.Mean = time.Duration(d.sumNs / float64(d.count))
	st.P50 = d.Quantile(0.50)
	st.P90 = d.Quantile(0.90)
	st.P95 = d.Quantile(0.95)
	st.P99 = d.Quantile(0.99)
	return st
}

func (d latencyDigest) Quantile(q float64) time.Duration {
	if d.count == 0 {
		return 0
	}
	if q <= 0 {
		return d.min
	}
	if q >= 1 {
		return d.max
	}
	rank := int(q * float64(d.count))
	if rank < 1 {
		rank = 1
	}
	keys := make([]int64, 0, len(d.buckets))
	for b := range d.buckets {
		keys = append(keys, b)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	seen := 0
	for _, b := range keys {
		seen += d.buckets[b]
		if seen >= rank {
			v := time.Duration(b)
			if v < d.min {
				return d.min
			}
			if v > d.max {
				return d.max
			}
			return v
		}
	}
	return d.max
}

func latencyBucketLower(ns int64) int64 {
	if ns <= 0 {
		return 0
	}
	scale := int64(1)
	for ns/scale >= 1000 {
		scale *= 10
	}
	return (ns / scale) * scale
}

type latencyStats struct {
	digest latencyDigest
	items  int
	errors int
}

func (s *latencyStats) AddSample(sample Sample, latency time.Duration) {
	s.items += sample.Items
	if sample.Err {
		s.errors++
		return
	}
	s.digest.Add(latency)
}

func (s *latencyStats) Merge(other latencyStats) {
	s.items += other.items
	s.errors += other.errors
	s.digest.Merge(other.digest)
}

func (s latencyStats) Stats() Stats {
	st := s.digest.Stats()
	st.Items = s.items
	st.Errors = s.errors
	return st
}

type countStatsAccumulator struct {
	values    map[int]int
	responses int
	empty     int
	min       int
	max       int
	total     int64
}

func (a *countStatsAccumulator) AddSample(s Sample) {
	if s.Err || s.ResultCount < 0 {
		return
	}
	a.Add(s.ResultCount)
}

func (a *countStatsAccumulator) Add(v int) {
	if a.values == nil {
		a.values = map[int]int{}
	}
	if a.responses == 0 {
		a.min = v
		a.max = v
	} else {
		if v < a.min {
			a.min = v
		}
		if v > a.max {
			a.max = v
		}
	}
	a.responses++
	if v == 0 {
		a.empty++
	}
	a.total += int64(v)
	a.values[v]++
}

func (a *countStatsAccumulator) Stats() *CountStats {
	if a.responses == 0 {
		return nil
	}
	cs := &CountStats{
		Responses: a.responses,
		Empty:     a.empty,
		Min:       a.min,
		Mean:      float64(a.total) / float64(a.responses),
		Max:       a.max,
		Total:     a.total,
	}
	cs.P50 = a.quantile(0.50)
	cs.P90 = a.quantile(0.90)
	cs.P99 = a.quantile(0.99)
	return cs
}

func (a *countStatsAccumulator) quantile(q float64) int {
	rank := int(q * float64(a.responses))
	if rank < 1 {
		rank = 1
	}
	keys := make([]int, 0, len(a.values))
	for v := range a.values {
		keys = append(keys, v)
	}
	sort.Ints(keys)
	seen := 0
	for _, v := range keys {
		seen += a.values[v]
		if seen >= rank {
			return v
		}
	}
	return a.max
}

type timelineStatsAccumulator struct {
	anchor  time.Time
	last    time.Time
	buckets map[int]*latencyStats
}

func (a *timelineStatsAccumulator) AddSample(s Sample) {
	if a.buckets == nil {
		a.buckets = map[int]*latencyStats{}
	}
	if a.anchor.IsZero() {
		a.anchor = s.Completed
		a.last = s.Completed
	} else {
		if s.Completed.Before(a.anchor) {
			a.rebase(s.Completed)
		}
		if s.Completed.After(a.last) {
			a.last = s.Completed
		}
	}
	idx := int(s.Completed.Sub(a.anchor) / timelineQuantum)
	if idx < 0 {
		idx = 0
	}
	a.bucket(idx).AddSample(s, s.Latency)
}

func (a *timelineStatsAccumulator) rebase(anchor time.Time) {
	oldAnchor := a.anchor
	rebased := map[int]*latencyStats{}
	for idx, st := range a.buckets {
		t := oldAnchor.Add(time.Duration(idx) * timelineQuantum)
		newIdx := int(t.Sub(anchor) / timelineQuantum)
		if newIdx < 0 {
			newIdx = 0
		}
		if rebased[newIdx] == nil {
			rebased[newIdx] = &latencyStats{}
		}
		rebased[newIdx].Merge(*st)
	}
	a.anchor = anchor
	a.buckets = rebased
}

func (a *timelineStatsAccumulator) bucket(idx int) *latencyStats {
	st := a.buckets[idx]
	if st == nil {
		st = &latencyStats{}
		a.buckets[idx] = st
	}
	return st
}

func (a timelineStatsAccumulator) Buckets() []TimelineBucket {
	if a.anchor.IsZero() {
		return nil
	}
	width := timelineWidth(a.last.Sub(a.anchor))
	if width < timelineQuantum {
		width = timelineQuantum
	}
	merged := map[int]*latencyStats{}
	maxIdx := int(a.last.Sub(a.anchor) / width)
	for qidx, st := range a.buckets {
		idx := int(time.Duration(qidx) * timelineQuantum / width)
		if idx < 0 {
			idx = 0
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		if merged[idx] == nil {
			merged[idx] = &latencyStats{}
		}
		merged[idx].Merge(*st)
	}
	out := make([]TimelineBucket, 0, maxIdx+1)
	for i := 0; i <= maxIdx; i++ {
		var st Stats
		if merged[i] != nil {
			st = merged[i].Stats()
		}
		sec := int((time.Duration(i) * width).Seconds())
		out = append(out, TimelineBucket{
			Offset:     "t+" + formatSeconds(sec),
			OffsetSec:  sec,
			Requests:   st.Count + st.Errors,
			Throughput: float64(st.Items) / width.Seconds(),
			P50:        st.P50,
			P99:        st.P99,
			Errors:     st.Errors,
		})
	}
	return out
}

func formatSeconds(sec int) string {
	return strconv.Itoa(sec) + "s"
}

type loadStats struct {
	totalSamples int

	overall       latencyStats
	response      latencyStats
	conditioned   latencyStats
	unconditioned latencyStats
	contextual    latencyStats
	noContextual  latencyStats
	byTarget      map[string]*latencyStats
	resultCounts  countStatsAccumulator
	timeline      timelineStatsAccumulator
}

func newLoadStats() *loadStats {
	return &loadStats{byTarget: map[string]*latencyStats{}}
}

func loadStatsFromSamples(samples []Sample) *loadStats {
	st := newLoadStats()
	for _, s := range samples {
		st.AddSample(s)
	}
	return st
}

func (s *loadStats) AddSample(sample Sample) {
	s.totalSamples++
	s.overall.AddSample(sample, sample.Latency)
	s.response.AddSample(sample, sample.RespLatency)
	if sample.Conditioned {
		s.conditioned.AddSample(sample, sample.Latency)
	} else {
		s.unconditioned.AddSample(sample, sample.Latency)
	}
	if sample.Contextual {
		s.contextual.AddSample(sample, sample.Latency)
	} else {
		s.noContextual.AddSample(sample, sample.Latency)
	}
	if s.byTarget == nil {
		s.byTarget = map[string]*latencyStats{}
	}
	if s.byTarget[sample.Target] == nil {
		s.byTarget[sample.Target] = &latencyStats{}
	}
	s.byTarget[sample.Target].AddSample(sample, sample.Latency)
	s.resultCounts.AddSample(sample)
	s.timeline.AddSample(sample)
}

func (s *loadStats) TotalSamples() int {
	if s == nil {
		return 0
	}
	return s.totalSamples
}
