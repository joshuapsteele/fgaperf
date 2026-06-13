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
