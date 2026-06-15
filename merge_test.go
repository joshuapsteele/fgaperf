package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildMergedReportCombinesDigestsAndRates(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	a := mergeFixtureReport(1, 4, 100, 200, start, []time.Duration{
		1 * time.Millisecond,
		1 * time.Millisecond,
	})
	b := mergeFixtureReport(2, 8, 200, 500, start.Add(10*time.Millisecond), []time.Duration{
		100 * time.Millisecond,
		100 * time.Millisecond,
	})
	pathA := writeReportFixture(t, dir, "a.json", a)
	pathB := writeReportFixture(t, dir, "b.json", b)

	merged, err := buildMergedReport([]string{pathA, pathB}, start)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Concurrency != 12 {
		t.Errorf("concurrency = %d, want 12", merged.Concurrency)
	}
	if merged.OfferedRate != 300 {
		t.Errorf("offered rate = %d, want 300", merged.OfferedRate)
	}
	if merged.AchievedRate != 700 {
		t.Errorf("achieved rate = %.0f, want 700", merged.AchievedRate)
	}
	if merged.Throughput != 700 {
		t.Errorf("throughput = %.0f, want 700", merged.Throughput)
	}
	if merged.Overall.Count != 4 {
		t.Fatalf("overall count = %d, want 4", merged.Overall.Count)
	}
	if merged.Overall.P50 != time.Millisecond {
		t.Errorf("merged p50 = %v, want 1ms", merged.Overall.P50)
	}
	if merged.Overall.P99 != 100*time.Millisecond {
		t.Errorf("merged p99 = %v, want 100ms", merged.Overall.P99)
	}
	if merged.ByTarget["document#viewer"].Count != 4 {
		t.Errorf("target count = %d, want 4", merged.ByTarget["document#viewer"].Count)
	}
	if merged.ResponseLatency == nil || merged.ResponseLatency.Count != 4 {
		t.Fatalf("response latency digest not merged: %+v", merged.ResponseLatency)
	}
	if len(merged.MergedFrom) != 2 || merged.MergedFrom[1].ClientID != 2 {
		t.Fatalf("merged inputs not recorded: %+v", merged.MergedFrom)
	}
	if merged.Digests == nil || merged.Digests.Overall.Count != 4 {
		t.Fatalf("merged report missing digests: %+v", merged.Digests)
	}
	md := merged.Markdown()
	if !strings.Contains(md, "merges 2 client-side result files") {
		t.Fatalf("merged markdown missing distributed note:\n%s", md)
	}
}

func TestMergeReportsWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	pathA := writeReportFixture(t, dir, "a.json", mergeFixtureReport(1, 1, 0, 10, start, []time.Duration{time.Millisecond}))
	pathB := writeReportFixture(t, dir, "b.json", mergeFixtureReport(2, 1, 0, 20, start, []time.Duration{2 * time.Millisecond}))

	jsonPath, mdPath, htmlPath, err := mergeReportsAt([]string{pathA, pathB}, dir, start)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(jsonPath) != "results-20260102-150405.json" {
		t.Fatalf("json artifact = %s", jsonPath)
	}
	if filepath.Base(mdPath) != "findings-20260102-150405.md" {
		t.Fatalf("markdown artifact = %s", mdPath)
	}
	if filepath.Base(htmlPath) != "report-20260102-150405.html" {
		t.Fatalf("HTML artifact = %s", htmlPath)
	}
	if _, err := LoadReport(jsonPath); err != nil {
		t.Fatalf("merged JSON did not load: %v", err)
	}
}

func TestMergeRejectsIncompatibleReports(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	a := mergeFixtureReport(1, 1, 0, 10, start, []time.Duration{time.Millisecond})
	b := mergeFixtureReport(2, 1, 0, 10, start, []time.Duration{time.Millisecond})
	b.CorpusSize = 99
	pathA := writeReportFixture(t, dir, "a.json", a)
	pathB := writeReportFixture(t, dir, "b.json", b)

	if _, err := buildMergedReport([]string{pathA, pathB}, start); err == nil {
		t.Fatal("incompatible reports merged without error")
	} else if !strings.Contains(err.Error(), "corpus_size differs") {
		t.Fatalf("error did not name incompatible field: %v", err)
	}
}

func TestMergeRejectsDifferentResolvedWorkloadConfig(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	a := mergeFixtureReport(1, 1, 0, 10, start, []time.Duration{time.Millisecond})
	b := mergeFixtureReport(2, 1, 0, 10, start, []time.Duration{time.Millisecond})
	a.ResolvedConfig["model_file"] = "model-a.json"
	b.ResolvedConfig["model_file"] = "model-b.json"
	pathA := writeReportFixture(t, dir, "a.json", a)
	pathB := writeReportFixture(t, dir, "b.json", b)

	if _, err := buildMergedReport([]string{pathA, pathB}, start); err == nil {
		t.Fatal("reports with different resolved workload configs merged without error")
	} else if !strings.Contains(err.Error(), "resolved workload config differs") || !strings.Contains(err.Error(), "model_file") {
		t.Fatalf("error did not name the resolved config difference: %v", err)
	}
}

// Two mixed-endpoint reports with the same blend merge into one whose
// per-endpoint counts are the sum of the inputs; two different blends are
// rejected.
func TestMergeMixedEndpointReports(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	a := mergeFixtureMixedReport(1, start)
	b := mergeFixtureMixedReport(2, start.Add(10*time.Millisecond))
	pathA := writeReportFixture(t, dir, "a.json", a)
	pathB := writeReportFixture(t, dir, "b.json", b)

	merged, err := buildMergedReport([]string{pathA, pathB}, start)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Endpoint != "mixed" {
		t.Fatalf("merged endpoint = %q, want mixed", merged.Endpoint)
	}
	if len(merged.ByEndpoint) != 2 {
		t.Fatalf("merged ByEndpoint = %+v, want 2 endpoints", merged.ByEndpoint)
	}
	// Each input contributed one check sample and one batch sample.
	if got := merged.ByEndpoint["check"].Count; got != 2 {
		t.Errorf("merged check count = %d, want 2", got)
	}
	if got := merged.ByEndpoint["batch-check"].Count; got != 2 {
		t.Errorf("merged batch-check count = %d, want 2", got)
	}
	if len(merged.EndpointMix) != 2 {
		t.Errorf("merged EndpointMix = %+v, want 2 shares", merged.EndpointMix)
	}

	// A report blended differently is not comparable.
	c := mergeFixtureMixedReport(3, start)
	c.EndpointMix = []EndpointShare{{"check", 90, 90}, {"batch-check", 10, 10}}
	pathC := writeReportFixture(t, dir, "c.json", c)
	if _, err := buildMergedReport([]string{pathA, pathC}, start); err == nil {
		t.Fatal("merged reports with different endpoint blends")
	} else if !strings.Contains(err.Error(), "endpoint_mix differs") {
		t.Fatalf("error did not name the endpoint_mix mismatch: %v", err)
	}
}

func TestClientIDChangesLoadRunSeed(t *testing.T) {
	a, _ := LoadConfigFile("")
	b, _ := LoadConfigFile("")
	b.Load.ClientID = 7
	if loadRunSeed(a) == loadRunSeed(b) {
		t.Fatal("different load.client_id values produced the same load RNG seed")
	}
}

func mergeFixtureReport(clientID, concurrency, rate int, achieved float64, start time.Time, latencies []time.Duration) *Report {
	st := newLoadStats()
	for i, latency := range latencies {
		st.AddSample(Sample{
			Target:      "document#viewer",
			Latency:     latency,
			RespLatency: latency + time.Millisecond,
			Completed:   start.Add(time.Duration(i) * 100 * time.Millisecond),
			Items:       1,
			ResultCount: -1,
		})
	}
	overall := st.overall.Stats()
	byTarget := map[string]Stats{}
	for target, ss := range st.byTarget {
		byTarget[target] = ss.Stats()
	}
	var response *Stats
	if rate > 0 {
		rl := st.response.Stats()
		response = &rl
	}
	return &Report{
		GeneratedAt:     start,
		ToolVersion:     toolVersion,
		APIURL:          "http://localhost:8080",
		Endpoint:        "check",
		Consistency:     "MINIMIZE_LATENCY",
		Concurrency:     concurrency,
		ClientID:        clientID,
		OfferedRate:     rate,
		Warmup:          "1s",
		Duration:        "10s",
		MeasuredWindow:  "10s",
		TupleCount:      10,
		CorpusSize:      5,
		CorpusDistinct:  5,
		TotalChecks:     int64(len(latencies)),
		Throughput:      achieved,
		AchievedRate:    achieved,
		Overall:         overall,
		ResponseLatency: response,
		ByTarget:        byTarget,
		Digests:         reportDigestsFromLoadStats(st, rate > 0),
		ResolvedConfig: map[string]any{
			"load":        map[string]any{"client_id": clientID, "concurrency": concurrency, "rate": rate},
			"random_seed": 42,
		},
	}
}

func mergeFixtureMixedReport(clientID int, start time.Time) *Report {
	st := newLoadStats()
	st.AddSample(Sample{Target: "document#viewer", Endpoint: "check", Latency: time.Millisecond, Completed: start, Items: 1, ResultCount: -1})
	st.AddSample(Sample{Target: "batch", Endpoint: "batch-check", Latency: 2 * time.Millisecond, Completed: start.Add(time.Millisecond), Items: 5, ResultCount: -1})
	byTarget := map[string]Stats{}
	for target, ss := range st.byTarget {
		byTarget[target] = ss.Stats()
	}
	byEndpoint := map[string]Stats{}
	for ep, ss := range st.byEndpoint {
		byEndpoint[ep] = ss.Stats()
	}
	return &Report{
		GeneratedAt:    start,
		ToolVersion:    toolVersion,
		APIURL:         "http://localhost:8080",
		Endpoint:       "mixed",
		EndpointMix:    []EndpointShare{{"batch-check", 50, 50}, {"check", 50, 50}},
		Consistency:    "MINIMIZE_LATENCY",
		Concurrency:    4,
		ClientID:       clientID,
		Warmup:         "1s",
		Duration:       "10s",
		MeasuredWindow: "10s",
		TupleCount:     10,
		CorpusSize:     5,
		CorpusDistinct: 5,
		TotalChecks:    6,
		Throughput:     6,
		Overall:        st.overall.Stats(),
		ByTarget:       byTarget,
		ByEndpoint:     byEndpoint,
		Digests:        reportDigestsFromLoadStats(st, false),
		ResolvedConfig: map[string]any{"random_seed": 42},
	}
}

func writeReportFixture(t *testing.T, dir, name string, r *Report) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
