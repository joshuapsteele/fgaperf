package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleReport() *Report {
	overallDigest := testStatsDigest(98, 2*time.Millisecond, 2, 8*time.Millisecond)
	targetDigest := testStatsDigest(48, 2*time.Millisecond, 2, 8*time.Millisecond)
	return &Report{
		Endpoint:       "check",
		Consistency:    "MINIMIZE_LATENCY",
		Concurrency:    16,
		Duration:       "60s",
		CorpusSize:     1000,
		CorpusDistinct: 800,
		Throughput:     5000,
		Overall:        Stats{Count: 100, Mean: 2 * time.Millisecond, P50: 2 * time.Millisecond, P99: 8 * time.Millisecond},
		ByTarget: map[string]Stats{
			"document#viewer": {Count: 50, P50: 2 * time.Millisecond, P99: 8 * time.Millisecond},
		},
		Digests: &ReportDigests{
			Overall:  overallDigest,
			ByTarget: map[string]StatsDigest{"document#viewer": targetDigest},
		},
		ResolvedConfig: map[string]any{
			"load":        map[string]any{"consistency": "MINIMIZE_LATENCY", "duration": "60s"},
			"random_seed": 1,
		},
	}
}

func testStatsDigest(firstN int, first time.Duration, secondN int, second time.Duration) StatsDigest {
	var st latencyStats
	for i := 0; i < firstN; i++ {
		st.AddSample(Sample{Items: 1}, first)
	}
	for i := 0; i < secondN; i++ {
		st.AddSample(Sample{Items: 1}, second)
	}
	return statsDigestFromLatencyStats(st)
}

func TestDiffConfigsFindsNestedDifference(t *testing.T) {
	a := sampleReport().ResolvedConfig
	b := sampleReport().ResolvedConfig
	b["load"].(map[string]any)["consistency"] = "HIGHER_CONSISTENCY"

	diffs := diffConfigs(a, b)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "load.consistency: MINIMIZE_LATENCY -> HIGHER_CONSISTENCY") {
		t.Errorf("diffs = %v", diffs)
	}
	if got := diffConfigs(a, a); len(got) != 0 {
		t.Errorf("identical configs produced diffs: %v", got)
	}
}

func TestCompareMarkdownShowsDeltasAndConfigDiff(t *testing.T) {
	a := sampleReport()
	b := sampleReport()
	b.Consistency = "HIGHER_CONSISTENCY"
	b.Overall.P99 = 12 * time.Millisecond
	b.ByTarget["document#viewer"] = Stats{Count: 50, P50: 3 * time.Millisecond, P99: 12 * time.Millisecond}
	b.ResolvedConfig["load"].(map[string]any)["consistency"] = "HIGHER_CONSISTENCY"

	md := CompareMarkdown("a.json", "b.json", a, b)
	for _, want := range []string{
		"load.consistency: MINIMIZE_LATENCY -> HIGHER_CONSISTENCY",
		"document#viewer",
		"+4.00 ms (+50.0%)", // overall p99 delta
	} {
		if !strings.Contains(md, want) {
			t.Errorf("compare markdown missing %q", want)
		}
	}
	if strings.Contains(md, "not directly comparable") {
		t.Error("comparable runs flagged with caveat")
	}
}

func TestCompareMarkdownCaveatsIncomparableRuns(t *testing.T) {
	a := sampleReport()
	b := sampleReport()
	b.Endpoint = "batch-check"
	b.Duration = "30s"
	b.OfferedRate = 500
	b.Warmup = "0s"
	b.WriteRate = 25
	b.Sweep = []SweepStep{{OfferedRate: 500}}

	md := CompareMarkdown("a.json", "b.json", a, b)
	for _, want := range []string{
		"not directly comparable",
		"endpoint differs",
		"offered rate differs",
		"warmup differs",
		"write churn differs",
		"sweep mode differs",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("compare markdown missing caveat %q:\n%s", want, md)
		}
	}
}

func TestCompareArtifactsDoNotOverwriteSameTimestamp(t *testing.T) {
	dir := t.TempDir()
	reportA := filepath.Join(dir, "a.json")
	reportB := filepath.Join(dir, "b.json")
	data, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportA, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportB, data, 0o644); err != nil {
		t.Fatal(err)
	}

	generatedAt := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	if err := compareAt(reportA, reportB, dir, generatedAt); err != nil {
		t.Fatal(err)
	}
	if err := compareAt(reportA, reportB, dir, generatedAt); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "compare-20260102-150405.md")
	second := filepath.Join(dir, "compare-20260102-150405-2.md")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("first compare artifact missing: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("second compare artifact missing: %v", err)
	}
}

func sweepResult(rate int, achievedFrac float64, respP99 time.Duration) *LoadResult {
	window := 10 * time.Second
	n := int(float64(rate) * achievedFrac * window.Seconds())
	samples := make([]Sample, n)
	base := time.Now()
	for i := range samples {
		samples[i] = Sample{
			Latency:     2 * time.Millisecond,
			RespLatency: respP99,
			Completed:   base.Add(time.Duration(i) * window / time.Duration(n)),
			Items:       1,
		}
	}
	return &LoadResult{
		OfferedRate:    rate,
		MeasuredWindow: window - window/time.Duration(n), // first to last completion
		Samples:        samples,
	}
}

func TestBuildSweepReportFindsKnee(t *testing.T) {
	cfg, _ := LoadConfigFile("")
	cfg.Load.SLOP99 = 50 * time.Millisecond
	corpus := &Corpus{Entries: []CorpusEntry{{Target: "a#r", User: "user:1", Relation: "r", Object: "a:1"}}}

	results := []*LoadResult{
		sweepResult(100, 1.0, 5*time.Millisecond),  // fine
		sweepResult(200, 1.0, 10*time.Millisecond), // fine: the knee
		sweepResult(400, 0.6, 2*time.Second),       // saturated
	}
	r := BuildSweepReport(results, corpus, cfg, 100, 0)
	if r.SweepKneeRate != 200 {
		t.Errorf("knee = %d, want 200", r.SweepKneeRate)
	}
	if len(r.Sweep) != 3 {
		t.Fatalf("got %d sweep steps, want 3", len(r.Sweep))
	}
	if !r.Sweep[0].PassesSLO {
		t.Error("5ms response p99 fails a 50ms SLO")
	}
	if !r.Sweep[2].Saturated {
		t.Error("step at 60% achieved not marked saturated")
	}
	if r.Sweep[2].PassesSLO {
		t.Error("step with 2s response p99 passes a 50ms SLO")
	}
	// headline reflects the knee step
	if r.OfferedRate != 200 {
		t.Errorf("headline offered rate = %d, want knee step 200", r.OfferedRate)
	}
	md := r.Markdown()
	if !strings.Contains(md, "## Rate sweep") || !strings.Contains(md, "◀ knee") {
		t.Error("sweep section or knee marker missing from markdown")
	}
}

func TestBuildSweepReportAllSaturated(t *testing.T) {
	cfg, _ := LoadConfigFile("")
	corpus := &Corpus{Entries: []CorpusEntry{{Target: "a#r"}}}
	results := []*LoadResult{sweepResult(1000, 0.5, time.Second)}
	r := BuildSweepReport(results, corpus, cfg, 100, 0)
	if r.SweepKneeRate != 0 {
		t.Errorf("knee = %d, want 0 when every step saturated", r.SweepKneeRate)
	}
	if !strings.Contains(r.Markdown(), "No step kept up") {
		t.Error("all-saturated callout missing")
	}
}
