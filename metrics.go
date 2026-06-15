package main

// metrics.go scrapes OpenFGA's Prometheus endpoint at the start and end of
// the measured phase and diffs the counters and histograms in between. The
// headline number this adds is datastore queries per check — the capacity
// currency for OpenFGA sizing that no client-side measurement can substitute
// for. Scraping happens outside the load hot path (a separate goroutine, a
// separate server port) and is best-effort: any failure degrades to the
// client-side-only report.

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The metric families we diff. Histograms are aggregated across all label
// sets: the snapshot diff isolates the measured phase, during which only load
// traffic flows, so per-label filtering would only make us fragile against
// upstream label changes.
var (
	histogramFamilies = []string{
		"openfga_request_duration_ms",
		"openfga_datastore_query_count",
		"openfga_dispatch_count",
	}
	counterFamilies = []string{
		"openfga_check_cache_hit_count",
		"openfga_check_cache_total_count",
	}
)

type histogram struct {
	Buckets map[float64]float64 // le upper bound -> cumulative count
	Sum     float64
	Count   float64
}

type snapshot struct {
	Histograms map[string]*histogram
	Counters   map[string]float64
}

type MetricsScraper struct {
	url  string
	http *http.Client
}

func NewMetricsScraper(url string) *MetricsScraper {
	if !strings.Contains(url, "/metrics") {
		url = strings.TrimRight(url, "/") + "/metrics"
	}
	return &MetricsScraper{url: url, http: &http.Client{Timeout: 5 * time.Second}}
}

func (m *MetricsScraper) Snapshot() (*snapshot, error) {
	resp, err := m.http.Get(m.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", m.url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePrometheus(string(data)), nil
}

// parsePrometheus extracts the families above from Prometheus text format,
// summing across label sets.
func parsePrometheus(text string) *snapshot {
	s := &snapshot{Histograms: map[string]*histogram{}, Counters: map[string]float64{}}
	hist := func(fam string) *histogram {
		h, ok := s.Histograms[fam]
		if !ok {
			h = &histogram{Buckets: map[float64]float64{}}
			s.Histograms[fam] = h
		}
		return h
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		for _, fam := range counterFamilies {
			if name == fam {
				s.Counters[fam] += value
			}
		}
		for _, fam := range histogramFamilies {
			switch name {
			case fam + "_bucket":
				if le, err := parseLE(labels); err == nil {
					hist(fam).Buckets[le] += value
				}
			case fam + "_sum":
				hist(fam).Sum += value
			case fam + "_count":
				hist(fam).Count += value
			}
		}
	}
	return s
}

// parseMetricLine splits `name{labels} value [timestamp]`. Label values are
// quoted and may contain escaped quotes, but the families we care about never
// do; a brace match is sufficient.
func parseMetricLine(line string) (name, labels string, value float64, ok bool) {
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", "", 0, false
		}
		name, labels, rest = line[:i], line[i+1:j], line[j+1:]
	} else if i := strings.IndexByte(line, ' '); i >= 0 {
		name, rest = line[:i], line[i:]
	} else {
		return "", "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

func parseLE(labels string) (float64, error) {
	for _, kv := range strings.Split(labels, ",") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(kv), `le="`); found {
			s := strings.TrimSuffix(rest, `"`)
			if s == "+Inf" {
				return math.Inf(1), nil
			}
			return strconv.ParseFloat(s, 64)
		}
	}
	return 0, fmt.Errorf("no le label in %q", labels)
}

// diff returns after - before, clamped at zero (counter resets mid-run would
// otherwise produce negative garbage).
//
// Caveat: a server restart resets these counters, so a clamped diff across a
// restart is meaningless — and because the clamp is applied per bucket, an
// uneven reset can also leave the bucket cumulative counts non-monotonic
// (a higher-le bucket below a lower-le one). We do not detect this here:
// a restart mid-run invalidates the whole measured phase, not just the
// server-side view, so the operator should re-run rather than trust either
// the client- or server-side numbers. The clamp's only job is to keep a
// transient scrape glitch from emitting negative buckets.
func (s *snapshot) diff(before *snapshot) *snapshot {
	out := &snapshot{Histograms: map[string]*histogram{}, Counters: map[string]float64{}}
	for fam, after := range s.Histograms {
		d := &histogram{Buckets: map[float64]float64{}}
		var b *histogram
		if before != nil {
			b = before.Histograms[fam]
		}
		if b == nil {
			b = &histogram{Buckets: map[float64]float64{}}
		}
		for le, v := range after.Buckets {
			d.Buckets[le] = math.Max(0, v-b.Buckets[le])
		}
		d.Sum = math.Max(0, after.Sum-b.Sum)
		d.Count = math.Max(0, after.Count-b.Count)
		out.Histograms[fam] = d
	}
	for fam, v := range s.Counters {
		var b float64
		if before != nil {
			b = before.Counters[fam]
		}
		out.Counters[fam] = math.Max(0, v-b)
	}
	return out
}

// HistogramSummary is a bucket-estimated view of a diffed histogram. The
// percentiles use linear interpolation within buckets, so their resolution is
// the server's bucket layout — coarser than the client-side percentiles.
type HistogramSummary struct {
	Count float64 `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
}

func summarizeHistogram(h *histogram) HistogramSummary {
	out := HistogramSummary{Count: h.Count}
	if h.Count == 0 {
		return out
	}
	out.Mean = h.Sum / h.Count
	out.P50 = histQuantile(h, 0.50)
	out.P90 = histQuantile(h, 0.90)
	out.P95 = histQuantile(h, 0.95)
	out.P99 = histQuantile(h, 0.99)
	return out
}

// histQuantile estimates a quantile from cumulative buckets with linear
// interpolation, the same way Prometheus' histogram_quantile does. A quantile
// landing in the +Inf bucket reports the highest finite bound.
func histQuantile(h *histogram, q float64) float64 {
	les := make([]float64, 0, len(h.Buckets))
	for le := range h.Buckets {
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 {
		return 0
	}
	total := h.Buckets[les[len(les)-1]]
	if total == 0 {
		return 0
	}
	rank := q * total
	var prevLE, prevCount float64
	for _, le := range les {
		c := h.Buckets[le]
		if c >= rank {
			if math.IsInf(le, 1) {
				return prevLE
			}
			if c == prevCount {
				return le
			}
			return prevLE + (le-prevLE)*(rank-prevCount)/(c-prevCount)
		}
		prevLE, prevCount = le, c
	}
	return prevLE
}

// ServerMetrics is the server-side view of the measured phase, diffed from
// the two snapshots.
type ServerMetrics struct {
	RequestDuration       HistogramSummary `json:"request_duration_ms"`
	DatastoreQueryCount   HistogramSummary `json:"datastore_query_count"` // per-request distribution
	DispatchCount         HistogramSummary `json:"dispatch_count"`        // per-request distribution
	TotalDatastoreQueries float64          `json:"total_datastore_queries"`
	CheckCacheHits        float64          `json:"check_cache_hits"`
	CheckCacheTotal       float64          `json:"check_cache_total"`
}

// dsQueryDiff returns the datastore-query histogram sum and count accumulated
// between two snapshots. The probe-time per-relation attribution pass diffs
// these around a small per-target batch to estimate mean datastore queries per
// check: sum/count is the average over every request the server recorded in
// the window. Best-effort — a missing family yields (0, 0).
func dsQueryDiff(before, after *snapshot) (sum, count float64) {
	d := after.diff(before)
	if h := d.Histograms["openfga_datastore_query_count"]; h != nil {
		return h.Sum, h.Count
	}
	return 0, 0
}

func buildServerMetrics(before, after *snapshot) *ServerMetrics {
	d := after.diff(before)
	sm := &ServerMetrics{
		CheckCacheHits:  d.Counters["openfga_check_cache_hit_count"],
		CheckCacheTotal: d.Counters["openfga_check_cache_total_count"],
	}
	if h := d.Histograms["openfga_request_duration_ms"]; h != nil {
		sm.RequestDuration = summarizeHistogram(h)
	}
	if h := d.Histograms["openfga_datastore_query_count"]; h != nil {
		sm.DatastoreQueryCount = summarizeHistogram(h)
		sm.TotalDatastoreQueries = h.Sum
	}
	if h := d.Histograms["openfga_dispatch_count"]; h != nil {
		sm.DispatchCount = summarizeHistogram(h)
	}
	return sm
}
