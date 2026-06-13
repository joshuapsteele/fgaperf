package main

// progress.go holds helpers for live, throttled progress output. Output is
// suppressed when stderr is not a terminal so CI logs and piped output stay
// clean.

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// isTerminal reports whether f is attached to a character device (a TTY).
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// fmtETA renders a remaining-time estimate as a compact human string.
func fmtETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

type probeProgress struct {
	total   int64
	done    int64
	allowed int64
	denied  int64

	mu      sync.Mutex
	current string
	start   time.Time
}

func newProbeProgress(total int) *probeProgress {
	if !isTerminal(os.Stderr) || total == 0 {
		return nil
	}
	return &probeProgress{total: int64(total), start: time.Now()}
}

func (p *probeProgress) setCurrent(target string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.current = target
	p.mu.Unlock()
}

func (p *probeProgress) add(allowed bool, classified bool) {
	if p == nil {
		return
	}
	if classified {
		if allowed {
			atomic.AddInt64(&p.allowed, 1)
		} else {
			atomic.AddInt64(&p.denied, 1)
		}
	}
	atomic.AddInt64(&p.done, 1)
}

func (p *probeProgress) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			fmt.Fprintf(os.Stderr, "\r%-96s\r", "")
			return
		case <-ticker.C:
			doneN := atomic.LoadInt64(&p.done)
			elapsed := time.Since(p.start).Seconds()
			rate := 0.0
			if elapsed > 0 {
				rate = float64(doneN) / elapsed
			}
			eta := "—"
			if rate > 0 && doneN < p.total {
				eta = fmtETA(time.Duration(float64(p.total-doneN)/rate) * time.Second)
			}
			p.mu.Lock()
			cur := p.current
			p.mu.Unlock()
			fmt.Fprintf(os.Stderr, "\rprobe: %d/%d checks | current %s | allowed/denied %d/%d | ETA %s",
				doneN, p.total, cur, atomic.LoadInt64(&p.allowed), atomic.LoadInt64(&p.denied), eta)
		}
	}
}

type loadProgress struct {
	start     time.Time
	warmupEnd time.Time
	deadline  time.Time

	mu      sync.Mutex
	items   int
	errors  int
	latency latencyStats
}

func newLoadProgress(start, warmupEnd, deadline time.Time) *loadProgress {
	if !isTerminal(os.Stderr) {
		return nil
	}
	return &loadProgress{start: start, warmupEnd: warmupEnd, deadline: deadline}
}

func (p *loadProgress) add(s Sample) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items += s.Items
	if s.Err {
		p.errors++
	}
	p.latency.AddSample(s, s.Latency)
}

func (p *loadProgress) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			fmt.Fprintf(os.Stderr, "\r%-96s\r", "")
			return
		case <-ticker.C:
			now := time.Now()
			if now.Before(p.warmupEnd) {
				fmt.Fprintf(os.Stderr, "\rload warmup: t+%s of %s | measured p99 -- | errors 0",
					fmtETA(now.Sub(p.start)), fmtETA(p.warmupEnd.Sub(p.start)))
				continue
			}
			elapsed := now.Sub(p.warmupEnd)
			if elapsed < 0 {
				elapsed = 0
			}
			total := p.deadline.Sub(p.warmupEnd)
			p.mu.Lock()
			items, errors := p.items, p.errors
			st := p.latency.Stats()
			p.mu.Unlock()
			rate := 0.0
			if elapsed > 0 {
				rate = float64(items) / elapsed.Seconds()
			}
			fmt.Fprintf(os.Stderr, "\rload: t+%s of %s | %.0f req/s | p99 %sms | %d errors",
				fmtETA(elapsed), fmtETA(total), rate, ms(st.P99), errors)
		}
	}
}
