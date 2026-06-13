package main

import (
	"testing"
	"time"
)

func TestTimelineWidth(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{0, time.Second},
		{5 * time.Second, time.Second},      // smoke run buckets by the second
		{60 * time.Second, 5 * time.Second}, // ~12 rows
		{10 * time.Minute, time.Minute},     // long run buckets by the minute
		{2 * time.Hour, time.Minute},        // capped at a minute
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
	// Each 1s bucket holds one 2-item sample => throughput 2/s.
	if tl[0].Throughput != 2 {
		t.Errorf("throughput = %v, want 2", tl[0].Throughput)
	}
	// p99 should climb across buckets (latency increases with time).
	if tl[14].P99 <= tl[0].P99 {
		t.Errorf("p99 should rise across the window: first=%v last=%v", tl[0].P99, tl[14].P99)
	}
	if buildTimeline(nil) != nil {
		t.Error("empty samples should produce a nil timeline")
	}
}
