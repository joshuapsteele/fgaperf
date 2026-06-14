package main

import (
	"testing"
	"time"
)

// Uniform arrival must stay byte-for-byte the even ticker: slot n at
// n*interval, so the existing fixed-rate output is unchanged.
func TestArrivalGenUniform(t *testing.T) {
	const rate = 100
	g := newArrivalGen("uniform", rate, 42)
	interval := time.Second / time.Duration(rate)
	for n := 0; n < 1000; n++ {
		got := g.next()
		want := time.Duration(n) * interval
		if got != want {
			t.Fatalf("uniform slot %d: got %v want %v", n, got, want)
		}
	}
}

// A fixed seed must reproduce the same poisson schedule, and that schedule must
// be monotonic non-decreasing so warmup gating and the intended-time accounting
// hold.
func TestArrivalGenPoissonDeterministic(t *testing.T) {
	a := newArrivalGen("poisson", 500, 12345)
	b := newArrivalGen("poisson", 500, 12345)
	var prev time.Duration
	for i := 0; i < 10000; i++ {
		x, y := a.next(), b.next()
		if x != y {
			t.Fatalf("poisson draw %d not deterministic: %v vs %v", i, x, y)
		}
		if x < prev {
			t.Fatalf("poisson schedule went backwards at draw %d: %v < %v", i, x, prev)
		}
		prev = x
	}
}

// Over many draws the mean achieved rate of the poisson schedule must converge
// on the offered rate (exponential inter-arrivals have mean 1/rate).
func TestArrivalGenPoissonMeanRate(t *testing.T) {
	const rate = 1000
	const n = 200000
	g := newArrivalGen("poisson", rate, 7)
	var last time.Duration
	for i := 0; i < n; i++ {
		last = g.next()
	}
	got := float64(n) / last.Seconds()
	if got < 0.97*rate || got > 1.03*rate {
		t.Fatalf("poisson mean rate %.1f not within 3%% of %d", got, rate)
	}
}

// The poisson schedule must actually differ from the uniform one; otherwise it
// would not exercise the bursty arrivals it exists to model.
func TestArrivalGenPoissonDiffersFromUniform(t *testing.T) {
	u := newArrivalGen("uniform", 500, 1)
	p := newArrivalGen("poisson", 500, 1)
	same := 0
	for i := 0; i < 100; i++ {
		if u.next() == p.next() {
			same++
		}
	}
	if same > 1 { // at most the n==0 offset could coincide
		t.Fatalf("poisson schedule matched uniform too often (%d/100)", same)
	}
}

func TestArrivalConfigDefaultAndValidation(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults(nil)
	if cfg.Load.Arrival != "uniform" {
		t.Fatalf("default arrival = %q, want uniform", cfg.Load.Arrival)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	cfg.Load.Arrival = "poisson"
	if err := cfg.validate(); err != nil {
		t.Fatalf("poisson arrival rejected: %v", err)
	}
	cfg.Load.Arrival = "bursty"
	if err := cfg.validate(); err == nil {
		t.Fatalf("invalid arrival %q accepted", cfg.Load.Arrival)
	}
}
