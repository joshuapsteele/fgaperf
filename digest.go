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

// StatsDigest is the JSON form of latencyStats. It keeps the mergeable latency
// sketch, item/error counts, and enough summary data to rebuild Stats without
// retaining raw samples.
type StatsDigest struct {
	Count   int           `json:"count"`
	Items   int           `json:"items"`
	Errors  int           `json:"errors"`
	SumNs   float64       `json:"sum_ns"`
	Min     time.Duration `json:"min_ns"`
	Max     time.Duration `json:"max_ns"`
	Buckets map[int64]int `json:"buckets,omitempty"`
}

func statsDigestFromLatencyStats(s latencyStats) StatsDigest {
	d := StatsDigest{
		Count:  s.digest.count,
		Items:  s.items,
		Errors: s.errors,
		SumNs:  s.digest.sumNs,
		Min:    s.digest.min,
		Max:    s.digest.max,
	}
	if len(s.digest.buckets) > 0 {
		d.Buckets = make(map[int64]int, len(s.digest.buckets))
		for b, n := range s.digest.buckets {
			d.Buckets[b] = n
		}
	}
	return d
}

func (d StatsDigest) latencyStats() latencyStats {
	s := latencyStats{
		items:  d.Items,
		errors: d.Errors,
		digest: latencyDigest{
			count:   d.Count,
			sumNs:   d.SumNs,
			min:     d.Min,
			max:     d.Max,
			buckets: map[int64]int{},
		},
	}
	if len(d.Buckets) > 0 {
		s.digest.buckets = make(map[int64]int, len(d.Buckets))
		for b, n := range d.Buckets {
			s.digest.buckets[b] = n
		}
	}
	return s
}

func (d StatsDigest) Stats() Stats {
	return d.latencyStats().Stats()
}

func (d StatsDigest) IsZero() bool {
	return d.Count == 0 && d.Items == 0 && d.Errors == 0 && len(d.Buckets) == 0
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

func (a *countStatsAccumulator) Merge(other countStatsAccumulator) {
	if other.responses == 0 {
		return
	}
	if a.values == nil {
		a.values = map[int]int{}
	}
	if a.responses == 0 {
		a.min = other.min
		a.max = other.max
	} else {
		if other.min < a.min {
			a.min = other.min
		}
		if other.max > a.max {
			a.max = other.max
		}
	}
	a.responses += other.responses
	a.empty += other.empty
	a.total += other.total
	for v, n := range other.values {
		a.values[v] += n
	}
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

type CountStatsDigest struct {
	Values    map[int]int `json:"values,omitempty"`
	Responses int         `json:"responses"`
	Empty     int         `json:"empty"`
	Min       int         `json:"min"`
	Max       int         `json:"max"`
	Total     int64       `json:"total"`
}

func countStatsDigestFromAccumulator(a countStatsAccumulator) CountStatsDigest {
	d := CountStatsDigest{
		Responses: a.responses,
		Empty:     a.empty,
		Min:       a.min,
		Max:       a.max,
		Total:     a.total,
	}
	if len(a.values) > 0 {
		d.Values = make(map[int]int, len(a.values))
		for v, n := range a.values {
			d.Values[v] = n
		}
	}
	return d
}

func (d CountStatsDigest) accumulator() countStatsAccumulator {
	a := countStatsAccumulator{
		responses: d.Responses,
		empty:     d.Empty,
		min:       d.Min,
		max:       d.Max,
		total:     d.Total,
		values:    map[int]int{},
	}
	if len(d.Values) > 0 {
		a.values = make(map[int]int, len(d.Values))
		for v, n := range d.Values {
			a.values[v] = n
		}
	}
	return a
}

func (d CountStatsDigest) IsZero() bool {
	return d.Responses == 0 && len(d.Values) == 0
}

type timelineStatsAccumulator struct {
	anchor  time.Time
	last    time.Time
	quantum time.Duration
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
	a.widenForWindow()
	q := a.effectiveQuantum()
	idx := int(s.Completed.Sub(a.anchor) / q)
	if idx < 0 {
		idx = 0
	}
	a.bucket(idx).AddSample(s, s.Latency)
}

func (a timelineStatsAccumulator) effectiveQuantum() time.Duration {
	if a.quantum > 0 {
		return a.quantum
	}
	return timelineQuantum
}

func (a *timelineStatsAccumulator) widenForWindow() {
	if a.anchor.IsZero() {
		return
	}
	q := timelineAccumulatorQuantum(a.last.Sub(a.anchor))
	if q > a.effectiveQuantum() {
		a.setQuantum(q)
	}
}

func timelineAccumulatorQuantum(window time.Duration) time.Duration {
	q := timelineWidth(window)
	if q < timelineQuantum {
		return timelineQuantum
	}
	return q
}

func (a *timelineStatsAccumulator) setQuantum(q time.Duration) {
	oldQ := a.effectiveQuantum()
	if q <= oldQ {
		if a.quantum == 0 {
			a.quantum = oldQ
		}
		return
	}
	rebucketed := map[int]*latencyStats{}
	for idx, st := range a.buckets {
		t := a.anchor.Add(time.Duration(idx) * oldQ)
		newIdx := int(t.Sub(a.anchor) / q)
		if newIdx < 0 {
			newIdx = 0
		}
		if rebucketed[newIdx] == nil {
			rebucketed[newIdx] = &latencyStats{}
		}
		rebucketed[newIdx].Merge(*st)
	}
	a.quantum = q
	a.buckets = rebucketed
}

func (a *timelineStatsAccumulator) rebase(anchor time.Time) {
	oldAnchor := a.anchor
	q := a.effectiveQuantum()
	rebased := map[int]*latencyStats{}
	for idx, st := range a.buckets {
		t := oldAnchor.Add(time.Duration(idx) * q)
		newIdx := int(t.Sub(anchor) / q)
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

func (a *timelineStatsAccumulator) Merge(other timelineStatsAccumulator) {
	if other.anchor.IsZero() {
		return
	}
	if a.anchor.IsZero() {
		a.anchor = other.anchor
		a.last = other.last
		a.quantum = other.effectiveQuantum()
		if len(other.buckets) > 0 {
			a.buckets = map[int]*latencyStats{}
			for idx, st := range other.buckets {
				copied := &latencyStats{}
				copied.Merge(*st)
				a.buckets[idx] = copied
			}
		}
		return
	}
	if other.anchor.Before(a.anchor) {
		a.rebase(other.anchor)
	}
	if other.last.After(a.last) {
		a.last = other.last
	}
	newQuantum := maxDuration(a.effectiveQuantum(), other.effectiveQuantum())
	newQuantum = maxDuration(newQuantum, timelineAccumulatorQuantum(a.last.Sub(a.anchor)))
	a.setQuantum(newQuantum)
	q := a.effectiveQuantum()
	otherQ := other.effectiveQuantum()
	for idx, st := range other.buckets {
		t := other.anchor.Add(time.Duration(idx) * otherQ)
		newIdx := int(t.Sub(a.anchor) / q)
		if newIdx < 0 {
			newIdx = 0
		}
		a.bucket(newIdx).Merge(*st)
	}
}

func (a timelineStatsAccumulator) Buckets() []TimelineBucket {
	if a.anchor.IsZero() {
		return nil
	}
	width := timelineWidth(a.last.Sub(a.anchor))
	q := a.effectiveQuantum()
	if width < q {
		width = q
	}
	merged := map[int]*latencyStats{}
	maxIdx := int(a.last.Sub(a.anchor) / width)
	for qidx, st := range a.buckets {
		idx := int(time.Duration(qidx) * q / width)
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

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

type TimelineDigest struct {
	Anchor    time.Time           `json:"anchor"`
	Last      time.Time           `json:"last"`
	QuantumNs int64               `json:"quantum_ns"`
	Buckets   map[int]StatsDigest `json:"buckets,omitempty"`
}

func timelineDigestFromAccumulator(a timelineStatsAccumulator) TimelineDigest {
	d := TimelineDigest{Anchor: a.anchor, Last: a.last, QuantumNs: a.effectiveQuantum().Nanoseconds()}
	if len(a.buckets) > 0 {
		d.Buckets = make(map[int]StatsDigest, len(a.buckets))
		for idx, st := range a.buckets {
			d.Buckets[idx] = statsDigestFromLatencyStats(*st)
		}
	}
	return d
}

func (d TimelineDigest) accumulator() timelineStatsAccumulator {
	a := timelineStatsAccumulator{anchor: d.Anchor, last: d.Last, quantum: time.Duration(d.QuantumNs)}
	if a.quantum <= 0 {
		a.quantum = timelineQuantum
	}
	if len(d.Buckets) > 0 {
		a.buckets = make(map[int]*latencyStats, len(d.Buckets))
		for idx, st := range d.Buckets {
			ls := st.latencyStats()
			a.buckets[idx] = &ls
		}
	}
	return a
}

func (d TimelineDigest) IsZero() bool {
	return d.Anchor.IsZero() && len(d.Buckets) == 0
}

func formatSeconds(sec int) string {
	return strconv.Itoa(sec) + "s"
}

// ReportDigests carries every mergeable population in a results JSON. Report
// keeps the human-friendly Stats fields; Digests lets later commands merge
// several reports without raw samples.
type ReportDigests struct {
	Overall       StatsDigest            `json:"overall"`
	Response      *StatsDigest           `json:"response,omitempty"`
	Conditioned   StatsDigest            `json:"conditioned"`
	Unconditioned StatsDigest            `json:"unconditioned"`
	Contextual    StatsDigest            `json:"contextual"`
	NoContextual  StatsDigest            `json:"without_contextual"`
	ByTarget      map[string]StatsDigest `json:"by_target,omitempty"`
	ResultCounts  *CountStatsDigest      `json:"result_counts,omitempty"`
	Timeline      *TimelineDigest        `json:"timeline,omitempty"`
	WriteChurn    *StatsDigest           `json:"write_churn,omitempty"`
}

func reportDigestsFromLoadStats(st *loadStats, includeResponse bool) *ReportDigests {
	if st == nil {
		return nil
	}
	d := &ReportDigests{
		Overall:       statsDigestFromLatencyStats(st.overall),
		Conditioned:   statsDigestFromLatencyStats(st.conditioned),
		Unconditioned: statsDigestFromLatencyStats(st.unconditioned),
		Contextual:    statsDigestFromLatencyStats(st.contextual),
		NoContextual:  statsDigestFromLatencyStats(st.noContextual),
		ByTarget:      map[string]StatsDigest{},
	}
	if includeResponse {
		resp := statsDigestFromLatencyStats(st.response)
		d.Response = &resp
	}
	for target, ss := range st.byTarget {
		d.ByTarget[target] = statsDigestFromLatencyStats(*ss)
	}
	counts := countStatsDigestFromAccumulator(st.resultCounts)
	if !counts.IsZero() {
		d.ResultCounts = &counts
	}
	timeline := timelineDigestFromAccumulator(st.timeline)
	if !timeline.IsZero() {
		d.Timeline = &timeline
	}
	return d
}

func (d *ReportDigests) loadStats() *loadStats {
	st := newLoadStats()
	if d == nil {
		return st
	}
	st.overall = d.Overall.latencyStats()
	st.totalSamples = st.overall.digest.count + st.overall.errors
	if d.Response != nil {
		st.response = d.Response.latencyStats()
	}
	st.conditioned = d.Conditioned.latencyStats()
	st.unconditioned = d.Unconditioned.latencyStats()
	st.contextual = d.Contextual.latencyStats()
	st.noContextual = d.NoContextual.latencyStats()
	for target, ss := range d.ByTarget {
		ls := ss.latencyStats()
		st.byTarget[target] = &ls
	}
	if d.ResultCounts != nil {
		st.resultCounts = d.ResultCounts.accumulator()
	}
	if d.Timeline != nil {
		st.timeline = d.Timeline.accumulator()
	}
	return st
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

func (s *loadStats) Merge(other *loadStats) {
	if other == nil {
		return
	}
	s.totalSamples += other.totalSamples
	s.overall.Merge(other.overall)
	s.response.Merge(other.response)
	s.conditioned.Merge(other.conditioned)
	s.unconditioned.Merge(other.unconditioned)
	s.contextual.Merge(other.contextual)
	s.noContextual.Merge(other.noContextual)
	if s.byTarget == nil {
		s.byTarget = map[string]*latencyStats{}
	}
	for target, ss := range other.byTarget {
		if s.byTarget[target] == nil {
			s.byTarget[target] = &latencyStats{}
		}
		s.byTarget[target].Merge(*ss)
	}
	s.resultCounts.Merge(other.resultCounts)
	s.timeline.Merge(other.timeline)
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
