package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
