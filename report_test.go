package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// fixedReport builds a representative single-run Report with every field set to
// a constant, so Markdown() renders deterministically for the golden test.
func fixedReport() *Report {
	st := func(count int, mean, p50, p90, p95, p99, max time.Duration) Stats {
		return Stats{Count: count, Items: count, Min: p50 / 2, Mean: mean, P50: p50, P90: p90, P95: p95, P99: p99, Max: max}
	}
	mssec := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	return &Report{
		GeneratedAt:    time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC),
		ToolVersion:    toolVersion,
		APIURL:         "http://localhost:8080",
		Endpoint:       "check",
		Consistency:    "MINIMIZE_LATENCY",
		Concurrency:    16,
		Warmup:         "10s",
		Duration:       "1m0s",
		MeasuredWindow: "1m0.012s",
		TupleCount:     2481,
		CorpusSize:     1000,
		CorpusDistinct: 666,
		TotalChecks:    300000,
		Mismatches:     0,
		VerifyResults:  true,
		Throughput:     4892,
		Overall:        st(294000, mssec(3), mssec(2), mssec(5), mssec(6), mssec(8), mssec(40)),
		Conditioned:    st(118000, mssec(4), mssec(3), mssec(6), mssec(7), mssec(10), mssec(38)),
		Unconditioned:  st(176000, mssec(2), mssec(2), mssec(4), mssec(5), mssec(7), mssec(40)),
		Contextual:     st(145000, mssec(3), mssec(3), mssec(6), mssec(7), mssec(9), mssec(30)),
		NoContextual:   st(149000, mssec(2), mssec(2), mssec(4), mssec(5), mssec(7), mssec(40)),
		ByTarget: map[string]Stats{
			"document#editor": st(98000, mssec(2), mssec(2), mssec(4), mssec(5), mssec(7), mssec(30)),
			"document#viewer": st(196000, mssec(3), mssec(3), mssec(6), mssec(7), mssec(9), mssec(40)),
		},
		ErrorsByClass: map[string]int64{"5xx": 1, "timeout": 2},
		ErrorSamples:  []string{"POST /stores/.../check: HTTP 500: internal error"},
		Server: &ServerMetrics{
			RequestDuration:       HistogramSummary{Count: 294000, Mean: 2.1, P50: 1.8, P90: 4.2, P95: 5.1, P99: 7.4},
			DatastoreQueryCount:   HistogramSummary{Count: 294000, Mean: 9.7, P95: 18, P99: 24},
			TotalDatastoreQueries: 2851800,
			CheckCacheHits:        120000,
			CheckCacheTotal:       294000,
		},
		Environment:  Environment{OS: "linux", Arch: "amd64", CPUs: 8, GoVersion: "go1.26.4"},
		SeedDuration: "97ms",
		SeedRate:     25577,
		Timeline: []TimelineBucket{
			{Offset: "t+0s", OffsetSec: 0, Requests: 4195, Throughput: 4195, P50: mssec(3), P99: mssec(16), Errors: 1},
			{Offset: "t+5s", OffsetSec: 5, Requests: 24700, Throughput: 4940, P50: mssec(2), P99: mssec(8), Errors: 0},
			{Offset: "t+10s", OffsetSec: 10, Requests: 24640, Throughput: 4928, P50: mssec(2), P99: mssec(8), Errors: 2},
		},
	}
}

// Report.Markdown() must render deterministically; the golden file guards
// against accidental formatting regressions. Refresh with `go test -update`.
func TestMarkdownGolden(t *testing.T) {
	got := fixedReport().Markdown()
	golden := filepath.Join("testdata", "findings.golden.md")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden (run `go test -update` to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("Markdown() output drifted from %s; re-run `go test -update` if intended.\n--- got (first 600 bytes) ---\n%.600s", golden, got)
	}
}

func TestTimelineWidth(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{0, time.Second},
		{5 * time.Second, time.Second},      // smoke run buckets by the second
		{60 * time.Second, 5 * time.Second}, // ~12 rows
		{10 * time.Minute, time.Minute},     // long run buckets by the minute
		{1 * time.Hour, 5 * time.Minute},    // soak run buckets by five-minute slices
		{2 * time.Hour, 10 * time.Minute},   // multi-hour windows stay roughly a dozen rows
		{24 * time.Hour, 2 * time.Hour},     // very long soaks keep widening
	}
	for _, c := range cases {
		if got := timelineWidth(c.window); got != c.want {
			t.Errorf("timelineWidth(%v) = %v, want %v", c.window, got, c.want)
		}
	}
}

func TestEndpointNoun(t *testing.T) {
	cases := map[string]string{
		"check":        "checks",
		"batch-check":  "checks",
		"list-objects": "list-objects calls",
		"list-users":   "list-users calls",
	}
	for endpoint, want := range cases {
		if got := endpointNoun(endpoint); got != want {
			t.Errorf("endpointNoun(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

// The per-relation table gains a DS column only when attribution ran; the
// default report (DSQueriesByTarget nil) omits it entirely.
func TestPerRelationDSColumn(t *testing.T) {
	r := fixedReport()
	if md := r.Markdown(); strings.Contains(md, "DS queries/check (probe) |") {
		t.Fatal("DS column rendered without attribution data")
	}

	r.DSQueriesByTarget = map[string]float64{
		"document#viewer": 3.2,
		"document#editor": 11.5,
	}
	md := r.Markdown()
	if !strings.Contains(md, "DS queries/check (probe) |") {
		t.Fatal("DS column missing from per-relation table when attribution data present")
	}
	if !strings.Contains(md, "| 11.5 |") || !strings.Contains(md, "| 3.2 |") {
		t.Errorf("per-relation DS values not rendered:\n%s", md)
	}

	// batch-check rows mix relations, so the DS column is omitted even when data
	// is present.
	r.Endpoint = "batch-check"
	r.ByTarget = map[string]Stats{"batch": {Count: 10, Items: 200, P99: 6 * time.Millisecond}}
	if md := r.Markdown(); strings.Contains(md, "DS queries/check (probe) |") {
		t.Error("DS column should be omitted for batch-check")
	}
}

func TestBatchCheckMarkdownLabelsBatchBreakdown(t *testing.T) {
	r := fixedReport()
	r.Endpoint = "batch-check"
	r.ByTarget = map[string]Stats{
		"batch": {Count: 10, Items: 200, P50: 2 * time.Millisecond, P95: 4 * time.Millisecond, P99: 6 * time.Millisecond},
	}
	md := r.Markdown()
	if strings.Contains(md, "## Per-relation breakdown") {
		t.Fatal("batch-check markdown should not label mixed batches as per-relation")
	}
	if !strings.Contains(md, "## Batch breakdown") {
		t.Fatal("batch-check markdown missing batch breakdown heading")
	}
}

func TestMarkdownSummaryAndHealthySuggestions(t *testing.T) {
	md := fixedReport().Markdown()
	for _, want := range []string{
		"## Summary",
		"Sustained 4892 checks/sec",
		"Client-side p99 was 8.00 ms.",
		"Verified responses had zero mismatches.",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("summary missing %q", want)
		}
	}
	if strings.Contains(md, "## Suggestions") {
		t.Fatal("healthy representative report should not render suggestions")
	}
}

func TestMarkdownDoesNotClaimVerificationWhenDisabled(t *testing.T) {
	r := fixedReport()
	r.VerifyResults = false
	r.ResolvedConfig = map[string]any{
		"load": map[string]any{"verify_results": false},
	}
	md := r.Markdown()
	for _, notWant := range []string{
		"Verified responses had zero mismatches.",
		"All verified responses matched probe-time expectations.",
	} {
		if strings.Contains(md, notWant) {
			t.Fatalf("markdown claimed verification success while verification was disabled: %q", notWant)
		}
	}
	if !strings.Contains(md, "Result verification was disabled.") {
		t.Fatalf("markdown did not explain disabled verification:\n%s", md)
	}
}

func TestMarkdownSuggestions(t *testing.T) {
	r := fixedReport()
	r.CorpusStats = map[string]CorpusTargetStats{
		"document#viewer": {Total: 30, Distinct: 10},
	}
	r.ResolvedConfig = map[string]any{
		"probe": map[string]any{"cohort_bias": 0.1},
	}
	md := r.Markdown()
	if !strings.Contains(md, "## Suggestions") {
		t.Fatal("suggestions section missing")
	}
	if !strings.Contains(md, "document#viewer") || !strings.Contains(md, "cohort_bias") {
		t.Fatalf("suggestions did not name duplication and cohort_bias:\n%s", md)
	}
}

// summarizeCounts must summarize list-endpoint result sizes and skip
// check-style samples (ResultCount < 0) and errored samples.
func TestSummarizeCounts(t *testing.T) {
	if summarizeCounts([]Sample{{ResultCount: -1}, {ResultCount: -1}}) != nil {
		t.Fatal("check-only samples should produce a nil distribution")
	}
	samples := []Sample{
		{ResultCount: 0},
		{ResultCount: 2},
		{ResultCount: 4},
		{ResultCount: 10},
		{ResultCount: 99, Err: true}, // errored: excluded
		{ResultCount: -1},            // not a list sample: excluded
	}
	cs := summarizeCounts(samples)
	if cs == nil {
		t.Fatal("expected a distribution")
	}
	if cs.Responses != 4 {
		t.Errorf("responses = %d, want 4", cs.Responses)
	}
	if cs.Empty != 1 {
		t.Errorf("empty = %d, want 1", cs.Empty)
	}
	if cs.Min != 0 || cs.Max != 10 {
		t.Errorf("min/max = %d/%d, want 0/10", cs.Min, cs.Max)
	}
	if cs.Total != 16 {
		t.Errorf("total = %d, want 16", cs.Total)
	}
	if cs.Mean != 4.0 {
		t.Errorf("mean = %v, want 4.0", cs.Mean)
	}
	if cs.P99 != 10 {
		t.Errorf("p99 = %d, want tail value 10", cs.P99)
	}
}

// buildTimeline must bucket samples by completion time anchored at the first
// measured sample, with throughput counting items (not just samples).
func TestBuildTimeline(t *testing.T) {
	base := time.Now()
	var samples []Sample
	// 15s window so bucket width is 1s: one sample/sec at increasing latency.
	for i := 0; i < 15; i++ {
		samples = append(samples, Sample{
			Completed: base.Add(time.Duration(i) * time.Second),
			Latency:   time.Duration(i+1) * time.Millisecond,
			Items:     2,
		})
	}
	samples = append(samples, Sample{
		Completed: base.Add(100 * time.Millisecond),
		Latency:   50 * time.Millisecond,
		Items:     1,
		Err:       true,
	})
	tl := buildTimeline(samples)
	if len(tl) != 15 {
		t.Fatalf("got %d buckets, want 15", len(tl))
	}
	if tl[0].OffsetSec != 0 || tl[14].OffsetSec != 14 {
		t.Errorf("offsets: first=%d last=%d", tl[0].OffsetSec, tl[14].OffsetSec)
	}
	if tl[0].Offset != "t+0s" {
		t.Errorf("offset label: %q", tl[0].Offset)
	}
	if tl[0].Requests != 2 {
		t.Errorf("requests = %d, want 2 including errored samples", tl[0].Requests)
	}
	if tl[0].Errors != 1 {
		t.Errorf("errors = %d, want 1", tl[0].Errors)
	}
	// The first 1s bucket holds one 2-item success and one 1-item error.
	if tl[0].Throughput != 3 {
		t.Errorf("throughput = %v, want 3", tl[0].Throughput)
	}
	// p99 should climb across buckets (latency increases with time).
	if tl[14].P99 <= tl[0].P99 {
		t.Errorf("p99 should rise across the window: first=%v last=%v", tl[0].P99, tl[14].P99)
	}
	if buildTimeline(nil) != nil {
		t.Error("empty samples should produce a nil timeline")
	}
}

func TestHTMLReportSelfContainedCharts(t *testing.T) {
	r := fixedReport()
	r.Sweep = []SweepStep{
		{OfferedRate: 1000, AchievedRate: 1000, Overall: r.Overall, ResponseLatency: r.Overall, PassesSLO: true},
		{OfferedRate: 2000, AchievedRate: 1960, Overall: r.Overall, ResponseLatency: r.Overall, PassesSLO: true},
		{OfferedRate: 3000, AchievedRate: 2200, Overall: r.Overall, ResponseLatency: r.Overall, Saturated: true, PassesSLO: false},
	}
	r.SweepKneeRate = 2000
	r.ResponseLatency = &Stats{Count: 294000, Items: 294000, Min: time.Millisecond, Mean: 4 * time.Millisecond, P50: 3 * time.Millisecond, P90: 8 * time.Millisecond, P95: 9 * time.Millisecond, P99: 12 * time.Millisecond, Max: 60 * time.Millisecond}
	r.ByTarget["document#<escaped&viewer>"] = Stats{Count: 10, Items: 10, Mean: time.Millisecond, P50: time.Millisecond, P95: 2 * time.Millisecond, P99: 3 * time.Millisecond, Max: 4 * time.Millisecond}

	html := r.HTML()
	for _, want := range []string{
		"<!doctype html>",
		"Rate sweep keep-up curve",
		"Latency over time",
		"Latency distribution",
		"Per-relation latency",
		"<svg",
		"document#&lt;escaped&amp;viewer&gt;",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q:\n%.800s", want, html)
		}
	}
	for _, forbidden := range []string{"<script", " src=", " href=", "@import"} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("HTML report is not self-contained; found %q", forbidden)
		}
	}
}

func TestReportSaveDoesNotOverwriteSameTimestamp(t *testing.T) {
	dir := t.TempDir()
	r1 := fixedReport()
	r1.mismatchRecords = []MismatchRecord{{User: "user:1", Relation: "viewer", Object: "document:1"}}
	json1, md1, html1, err := r1.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	mm1 := r1.MismatchFile

	r2 := fixedReport()
	r2.mismatchRecords = []MismatchRecord{{User: "user:2", Relation: "viewer", Object: "document:2"}}
	json2, md2, html2, err := r2.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	mm2 := r2.MismatchFile

	for _, pair := range [][2]string{{json1, json2}, {md1, md2}, {html1, html2}, {mm1, mm2}} {
		if pair[0] == pair[1] {
			t.Fatalf("artifact path reused: %s", pair[0])
		}
		for _, path := range pair {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("artifact %s missing: %v", path, err)
			}
		}
	}
}
