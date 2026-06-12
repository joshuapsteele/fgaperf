package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "deadline exceeded" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&HTTPError{StatusCode: 429}, "4xx"},
		{&HTTPError{StatusCode: 503}, "5xx"},
		{fakeTimeoutErr{}, "timeout"},
		{&json.SyntaxError{}, "decode"},
		{errors.New("connection refused"), "connection"},
		{fmt.Errorf("wrapped: %w", &HTTPError{StatusCode: 500}), "5xx"},
	}
	for _, c := range cases {
		if got := classifyErr(c.err); got != c.want {
			t.Errorf("classifyErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Without weights the picker must stay uniform over entries; with weights the
// per-target request share must follow the weights even when group sizes
// differ.
func TestCorpusPickerWeights(t *testing.T) {
	corpus := &Corpus{Entries: []CorpusEntry{
		{Target: "a#r", Object: "a:1"},
		{Target: "a#r", Object: "a:2"},
		{Target: "a#r", Object: "a:3"},
		{Target: "b#r", Object: "b:1"},
	}}
	rng := rand.New(rand.NewSource(1))

	uniform := newCorpusPicker(corpus)
	counts := map[string]int{}
	for i := 0; i < 40000; i++ {
		counts[uniform.pick(rng).Target]++
	}
	if frac := float64(counts["a#r"]) / 40000; frac < 0.72 || frac > 0.78 {
		t.Errorf("uniform picker: a#r share = %.3f, want ~0.75 (uniform over entries)", frac)
	}

	corpus.Weights = map[string]float64{"a#r": 1, "b#r": 3}
	weighted := newCorpusPicker(corpus)
	counts = map[string]int{}
	for i := 0; i < 40000; i++ {
		counts[weighted.pick(rng).Target]++
	}
	if frac := float64(counts["b#r"]) / 40000; frac < 0.72 || frac > 0.78 {
		t.Errorf("weighted picker: b#r share = %.3f, want ~0.75 (weight 3 of 4)", frac)
	}
}

func TestMismatchRecorderDedupsAndCaps(t *testing.T) {
	r := newMismatchRecorder()
	e := &CorpusEntry{User: "user:1", Relation: "viewer", Object: "doc:1", Target: "doc#viewer", Expected: true}
	r.record(e, false)
	r.record(e, false) // duplicate
	if len(r.out) != 1 {
		t.Fatalf("got %d records, want 1 after dedup", len(r.out))
	}
	if r.out[0].Expected != true || r.out[0].Observed != false {
		t.Errorf("record = %+v", r.out[0])
	}
	for i := 0; i < 2*maxMismatchRecords; i++ {
		r.record(&CorpusEntry{User: fmt.Sprintf("user:%d", i), Relation: "viewer", Object: "doc:1"}, false)
	}
	if len(r.out) != maxMismatchRecords {
		t.Errorf("got %d records, want cap %d", len(r.out), maxMismatchRecords)
	}
}

// Response latency must be summarized independently of service latency so the
// fixed-rate report can show queueing delay.
func TestSummarizeResponseUsesRespLatency(t *testing.T) {
	samples := []Sample{
		{Latency: 1 * time.Millisecond, RespLatency: 100 * time.Millisecond, Items: 1},
		{Latency: 2 * time.Millisecond, RespLatency: 200 * time.Millisecond, Items: 1},
	}
	svc := Summarize(samples)
	resp := SummarizeResponse(samples)
	if svc.Max != 2*time.Millisecond {
		t.Errorf("service max = %v, want 2ms", svc.Max)
	}
	if resp.Max != 200*time.Millisecond {
		t.Errorf("response max = %v, want 200ms", resp.Max)
	}
}
