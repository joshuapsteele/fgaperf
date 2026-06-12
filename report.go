package main

// report.go writes machine-readable results plus a findings document skeleton
// with the measured numbers filled in.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Report struct {
	GeneratedAt   time.Time        `json:"generated_at"`
	ToolVersion   string           `json:"tool_version"`
	APIURL        string           `json:"api_url"`
	Endpoint      string           `json:"endpoint"`
	Consistency   string           `json:"consistency"`
	Concurrency   int              `json:"concurrency"`
	OfferedRate   int              `json:"offered_rate"`
	Warmup        string           `json:"warmup"`
	Duration      string           `json:"duration"`
	TupleCount    int              `json:"tuple_count"`
	CorpusSize    int              `json:"corpus_size"`
	TotalChecks   int64            `json:"total_checks_incl_warmup"`
	Mismatches    int64            `json:"result_mismatches"`
	Throughput    float64          `json:"throughput_per_sec"`
	Overall       Stats            `json:"overall"`
	Conditioned   Stats            `json:"conditioned"`
	Unconditioned Stats            `json:"unconditioned"`
	ByTarget      map[string]Stats `json:"by_target"`
	SeedDuration  string           `json:"seed_duration,omitempty"`
	SeedRate      float64          `json:"seed_tuples_per_sec,omitempty"`
}

const toolVersion = "0.1.0"

func BuildReport(res *LoadResult, corpus *Corpus, cfg *Config, tupleCount int, seedDur time.Duration) *Report {
	r := &Report{
		GeneratedAt: time.Now().UTC(),
		ToolVersion: toolVersion,
		APIURL:      cfg.OpenFGA.APIURL,
		Endpoint:    res.Endpoint,
		Consistency: res.Consistency,
		Concurrency: res.Concurrency,
		OfferedRate: res.OfferedRate,
		Warmup:      res.Warmup.String(),
		Duration:    res.Duration.String(),
		TupleCount:  tupleCount,
		CorpusSize:  len(corpus.Entries),
		TotalChecks: res.TotalChecks,
		Mismatches:  res.Mismatches,
		ByTarget:    map[string]Stats{},
	}
	if seedDur > 0 {
		r.SeedDuration = seedDur.String()
		r.SeedRate = float64(tupleCount) / seedDur.Seconds()
	}
	var cond, uncond []Sample
	byTarget := map[string][]Sample{}
	items := 0
	for _, s := range res.Samples {
		items += s.Items
		if s.Conditioned {
			cond = append(cond, s)
		} else {
			uncond = append(uncond, s)
		}
		byTarget[s.Target] = append(byTarget[s.Target], s)
	}
	r.Overall = Summarize(res.Samples)
	r.Conditioned = Summarize(cond)
	r.Unconditioned = Summarize(uncond)
	for t, ss := range byTarget {
		r.ByTarget[t] = Summarize(ss)
	}
	if res.Duration > 0 {
		r.Throughput = float64(items) / res.Duration.Seconds()
	}
	return r
}

func (r *Report) Save(dir string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := r.GeneratedAt.Format("20060102-150405")
	jsonPath := filepath.Join(dir, "results-"+stamp+".json")
	mdPath := filepath.Join(dir, "findings-"+stamp+".md")
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(r.Markdown()), 0o644); err != nil {
		return "", "", err
	}
	return jsonPath, mdPath, nil
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000.0)
}

func (r *Report) Markdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# OpenFGA Performance Test Findings")
	w("")
	w("Generated %s by fgaperf %s. All latencies in milliseconds, measured client-side over HTTP against %s.",
		r.GeneratedAt.Format("2006-01-02 15:04 UTC"), r.ToolVersion, r.APIURL)
	w("")
	w("## Test configuration")
	w("")
	w("| Parameter | Value |")
	w("|---|---|")
	w("| Endpoint | %s |", r.Endpoint)
	w("| Consistency | %s |", r.Consistency)
	w("| Concurrency | %d workers |", r.Concurrency)
	if r.OfferedRate > 0 {
		w("| Offered rate | %d req/s |", r.OfferedRate)
	} else {
		w("| Offered rate | closed loop |")
	}
	w("| Warmup / measured | %s / %s |", r.Warmup, r.Duration)
	w("| Seeded tuples | %d |", r.TupleCount)
	w("| Check corpus | %d entries |", r.CorpusSize)
	w("| Client | %s, %d CPU |", runtime.GOOS+"/"+runtime.GOARCH, runtime.NumCPU())
	w("")
	w("## Headline results")
	w("")
	w("Sustained throughput was %.0f checks/sec with %d errors out of %d measured requests. %s",
		r.Throughput, r.Overall.Errors, r.Overall.Count+r.Overall.Errors, mismatchSentence(r.Mismatches))
	w("")
	w("| Population | Requests | Mean | p50 | p90 | p95 | p99 | Max |")
	w("|---|---|---|---|---|---|---|---|")
	row := func(name string, s Stats) {
		if s.Count == 0 {
			return
		}
		w("| %s | %d | %s | %s | %s | %s | %s | %s |",
			name, s.Count, ms(s.Mean), ms(s.P50), ms(s.P90), ms(s.P95), ms(s.P99), ms(s.Max))
	}
	row("All checks", r.Overall)
	row("CEL-conditioned paths", r.Conditioned)
	row("Unconditioned paths", r.Unconditioned)
	w("")
	if r.Conditioned.Count > 0 && r.Unconditioned.Count > 0 {
		deltaP50 := float64(r.Conditioned.P50-r.Unconditioned.P50) / float64(time.Millisecond)
		deltaP99 := float64(r.Conditioned.P99-r.Unconditioned.P99) / float64(time.Millisecond)
		w("Checks whose resolution path can evaluate a CEL condition ran %.2f ms slower at p50 and %.2f ms slower at p99 than checks on unconditioned relations. Note that conditioned and unconditioned populations also differ in graph depth, so this delta is an upper bound on pure CEL evaluation cost; compare relations of similar depth in the per-relation table below for a tighter read.", deltaP50, deltaP99)
		w("")
	}
	w("## Per-relation breakdown")
	w("")
	w("| Relation | Requests | Mean | p50 | p95 | p99 |")
	w("|---|---|---|---|---|---|")
	targets := make([]string, 0, len(r.ByTarget))
	for t := range r.ByTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		s := r.ByTarget[t]
		if s.Count == 0 {
			continue
		}
		w("| %s | %d | %s | %s | %s | %s |", t, s.Count, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99))
	}
	w("")
	if r.SeedRate > 0 {
		w("## Write path")
		w("")
		w("Seeding %d tuples took %s, a sustained write rate of %.0f tuples/sec using transactional Write calls.",
			r.TupleCount, r.SeedDuration, r.SeedRate)
		w("")
	}
	w("## Caveats and interpretation")
	w("")
	w("Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.")
	return b.String()
}

func mismatchSentence(n int64) string {
	if n == 0 {
		return "All verified responses matched probe-time expectations."
	}
	return fmt.Sprintf("%d responses differed from probe-time expectations (investigate cache staleness or consistency settings).", n)
}
