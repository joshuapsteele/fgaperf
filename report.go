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
	GeneratedAt     time.Time        `json:"generated_at"`
	ToolVersion     string           `json:"tool_version"`
	APIURL          string           `json:"api_url"`
	Endpoint        string           `json:"endpoint"`
	Consistency     string           `json:"consistency"`
	Concurrency     int              `json:"concurrency"`
	OfferedRate     int              `json:"offered_rate"`
	AchievedRate    float64          `json:"achieved_rate_per_sec,omitempty"` // fixed-rate only: measured requests / measured window
	DroppedSlots    int64            `json:"dropped_rate_slots,omitempty"`
	Warmup          string           `json:"warmup"`
	Duration        string           `json:"duration"`
	MeasuredWindow  string           `json:"measured_window"` // first to last sample completion
	TupleCount      int              `json:"tuple_count"`
	CorpusSize      int              `json:"corpus_size"`
	CorpusDistinct  int              `json:"corpus_distinct"`
	TotalChecks     int64            `json:"total_checks_incl_warmup"`
	Mismatches      int64            `json:"result_mismatches"`
	Throughput      float64          `json:"throughput_per_sec"`
	Overall         Stats            `json:"overall"`
	ResponseLatency *Stats           `json:"response_latency,omitempty"` // fixed-rate only: intended send -> response
	Conditioned     Stats            `json:"conditioned"`
	Unconditioned   Stats            `json:"unconditioned"`
	Contextual      Stats            `json:"contextual"`
	NoContextual    Stats            `json:"without_contextual"`
	ByTarget        map[string]Stats `json:"by_target"`
	ErrorsByClass   map[string]int64 `json:"errors_by_class,omitempty"`
	ErrorSamples    []string         `json:"error_samples,omitempty"`
	Server          *ServerMetrics   `json:"server,omitempty"` // diffed Prometheus view of the measured phase
	SeedDuration    string           `json:"seed_duration,omitempty"`
	SeedRate        float64          `json:"seed_tuples_per_sec,omitempty"`
	Environment     Environment      `json:"environment"`
	ResolvedConfig  map[string]any   `json:"resolved_config,omitempty"` // post-defaults config, credentials redacted
}

// Environment records where the client side of the measurement ran.
type Environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go_version"`
}

const toolVersion = "0.1.0"

func BuildReport(res *LoadResult, corpus *Corpus, cfg *Config, tupleCount int, seedDur time.Duration) *Report {
	r := &Report{
		GeneratedAt:    time.Now().UTC(),
		ToolVersion:    toolVersion,
		APIURL:         cfg.OpenFGA.APIURL,
		Endpoint:       res.Endpoint,
		Consistency:    res.Consistency,
		Concurrency:    res.Concurrency,
		OfferedRate:    res.OfferedRate,
		DroppedSlots:   res.DroppedSlots,
		Warmup:         res.Warmup.String(),
		Duration:       res.Duration.String(),
		MeasuredWindow: res.MeasuredWindow.Round(time.Millisecond).String(),
		TupleCount:     tupleCount,
		CorpusSize:     len(corpus.Entries),
		CorpusDistinct: corpus.Distinct(),
		TotalChecks:    res.TotalChecks,
		Mismatches:     res.Mismatches,
		ByTarget:       map[string]Stats{},
		ErrorsByClass:  res.ErrorsByClass,
		ErrorSamples:   res.ErrorSamples,
		Server:         res.Server,
		Environment: Environment{
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			CPUs:      runtime.NumCPU(),
			GoVersion: runtime.Version(),
		},
		ResolvedConfig: cfg.Resolved(),
	}
	if seedDur > 0 {
		r.SeedDuration = seedDur.String()
		r.SeedRate = float64(tupleCount) / seedDur.Seconds()
	}
	var cond, uncond, contextual, noContextual []Sample
	byTarget := map[string][]Sample{}
	items := 0
	for _, s := range res.Samples {
		items += s.Items
		if s.Conditioned {
			cond = append(cond, s)
		} else {
			uncond = append(uncond, s)
		}
		if s.Contextual {
			contextual = append(contextual, s)
		} else {
			noContextual = append(noContextual, s)
		}
		byTarget[s.Target] = append(byTarget[s.Target], s)
	}
	r.Overall = Summarize(res.Samples)
	r.Conditioned = Summarize(cond)
	r.Unconditioned = Summarize(uncond)
	r.Contextual = Summarize(contextual)
	r.NoContextual = Summarize(noContextual)
	for t, ss := range byTarget {
		r.ByTarget[t] = Summarize(ss)
	}
	// Throughput over the wall clock the samples actually spanned, not the
	// configured duration: in-flight requests complete after the deadline and
	// slow tails stretch the real window.
	window := res.MeasuredWindow.Seconds()
	if window <= 0 {
		window = res.Duration.Seconds()
	}
	if window > 0 {
		r.Throughput = float64(items) / window
	}
	if res.OfferedRate > 0 {
		if window > 0 {
			r.AchievedRate = float64(len(res.Samples)) / window
		}
		rl := SummarizeResponse(res.Samples)
		r.ResponseLatency = &rl
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
	w("| Warmup / measured | %s / %s (actual window %s) |", r.Warmup, r.Duration, r.MeasuredWindow)
	w("| Seeded tuples | %d |", r.TupleCount)
	w("| Check corpus | %d entries (%d distinct checks) |", r.CorpusSize, r.CorpusDistinct)
	w("| Client | %s, %d CPU |", runtime.GOOS+"/"+runtime.GOARCH, runtime.NumCPU())
	w("")
	w("## Headline results")
	w("")
	w("Sustained throughput was %.0f checks/sec over the %s measured window, with %d errors out of %d measured requests. %s",
		r.Throughput, r.MeasuredWindow, r.Overall.Errors, r.Overall.Count+r.Overall.Errors, mismatchSentence(r.Mismatches))
	w("")
	if r.OfferedRate > 0 {
		w("Achieved request rate was %.0f req/s against an offered %d req/s, with %d rate slots dropped because all workers were busy.",
			r.AchievedRate, r.OfferedRate, r.DroppedSlots)
		w("")
		if r.AchievedRate < 0.98*float64(r.OfferedRate) {
			w("> **⚠️ The server did not keep up with the offered rate** (achieved %.0f of %d req/s). Service-latency percentiles below understate what a caller would experience; read the response-latency row, which measures from each request's *scheduled* send time and therefore includes the queueing delay a saturated server imposes.",
				r.AchievedRate, r.OfferedRate)
			w("")
		}
	}
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
	if r.ResponseLatency != nil {
		row("All checks (response latency)", *r.ResponseLatency)
	}
	row("CEL-conditioned paths", r.Conditioned)
	row("Unconditioned paths", r.Unconditioned)
	row("With contextual tuples", r.Contextual)
	row("Without contextual tuples", r.NoContextual)
	w("")
	if r.ResponseLatency != nil {
		w("All rows except \"response latency\" measure service latency (request start to response). Response latency measures from each request's scheduled send time under the offered rate, so it additionally captures time spent waiting for a free worker — the coordinated-omission-corrected view.")
		w("")
	}
	if r.Conditioned.Count > 0 && r.Unconditioned.Count > 0 {
		deltaP50 := float64(r.Conditioned.P50-r.Unconditioned.P50) / float64(time.Millisecond)
		deltaP99 := float64(r.Conditioned.P99-r.Unconditioned.P99) / float64(time.Millisecond)
		w("Checks whose resolution path can evaluate a CEL condition ran %.2f ms slower at p50 and %.2f ms slower at p99 than checks on unconditioned relations. Note that conditioned and unconditioned populations also differ in graph depth, so this delta is an upper bound on pure CEL evaluation cost; compare relations of similar depth in the per-relation table below for a tighter read.", deltaP50, deltaP99)
		w("")
	}
	if r.Contextual.Count > 0 && r.NoContextual.Count > 0 {
		deltaP50 := float64(r.Contextual.P50-r.NoContextual.P50) / float64(time.Millisecond)
		deltaP99 := float64(r.Contextual.P99-r.NoContextual.P99) / float64(time.Millisecond)
		w("Checks carrying contextual tuples ran %.2f ms slower at p50 and %.2f ms slower at p99 than checks without contextual tuples. This split reflects the configured corpus mix; compare the same target relation with and without contextual assertions for the cleanest read.", deltaP50, deltaP99)
		w("")
	}
	w("## Per-relation breakdown")
	w("")
	w("| Relation | Requests | Errors | Mean | p50 | p95 | p99 |")
	w("|---|---|---|---|---|---|---|")
	targets := make([]string, 0, len(r.ByTarget))
	for t := range r.ByTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		s := r.ByTarget[t]
		if s.Count == 0 && s.Errors == 0 {
			continue
		}
		w("| %s | %d | %d | %s | %s | %s | %s |", t, s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99))
	}
	w("")
	if len(r.ErrorsByClass) > 0 {
		w("## Errors")
		w("")
		w("| Class | Count |")
		w("|---|---|")
		classes := make([]string, 0, len(r.ErrorsByClass))
		for c := range r.ErrorsByClass {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for _, c := range classes {
			w("| %s | %d |", c, r.ErrorsByClass[c])
		}
		w("")
		if len(r.ErrorSamples) > 0 {
			w("First error messages observed:")
			w("")
			for _, e := range r.ErrorSamples {
				w("- `%s`", e)
			}
			w("")
		}
	}
	if r.Server != nil && r.Server.RequestDuration.Count > 0 {
		s := r.Server
		w("## Server-side view")
		w("")
		w("Diffed from OpenFGA's Prometheus metrics between the start and end of the measured phase. Percentiles are estimated from histogram buckets, so they are coarser than the client-side numbers above.")
		w("")
		w("| Metric | Value |")
		w("|---|---|")
		w("| Server-side request duration | mean %.2f ms, p50 %.2f, p95 %.2f, p99 %.2f |",
			s.RequestDuration.Mean, s.RequestDuration.P50, s.RequestDuration.P95, s.RequestDuration.P99)
		w("| Server-side requests observed | %.0f |", s.RequestDuration.Count)
		if s.DatastoreQueryCount.Count > 0 {
			w("| Datastore queries per request | mean %.2f, p95 %.0f, p99 %.0f |",
				s.DatastoreQueryCount.Mean, s.DatastoreQueryCount.P95, s.DatastoreQueryCount.P99)
			w("| Total datastore queries | %.0f |", s.TotalDatastoreQueries)
		}
		if s.DispatchCount.Count > 0 {
			w("| Dispatches per request | mean %.2f, p95 %.0f, p99 %.0f |",
				s.DispatchCount.Mean, s.DispatchCount.P95, s.DispatchCount.P99)
		}
		if s.CheckCacheTotal > 0 {
			w("| Check cache hit rate | %.1f%% of %.0f lookups |", 100*s.CheckCacheHits/s.CheckCacheTotal, s.CheckCacheTotal)
		}
		w("")
		w("Datastore queries per request is the capacity currency for OpenFGA sizing: it tells you how much database load each check translates into, independent of network and JSON overhead.")
		w("")
	}
	if r.SeedRate > 0 {
		w("## Write path")
		w("")
		w("Seeding %d tuples took %s, a sustained write rate of %.0f tuples/sec using transactional Write calls.",
			r.TupleCount, r.SeedDuration, r.SeedRate)
		w("")
	}
	w("## Caveats and interpretation")
	w("")
	if r.CorpusSize > 0 && r.CorpusDistinct > 0 {
		dup := float64(r.CorpusSize) / float64(r.CorpusDistinct)
		w("The corpus replays %d distinct checks across %d entries (%.1fx average duplication). Duplication inflates server-side cache hit rates relative to production traffic; if it is high, lower probe.allowed_ratio pressure (or raise probe.samples_per_target) and rerun.", r.CorpusDistinct, r.CorpusSize, dup)
		w("")
	}
	w("Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.")
	return b.String()
}

func mismatchSentence(n int64) string {
	if n == 0 {
		return "All verified responses matched probe-time expectations."
	}
	return fmt.Sprintf("%d responses differed from probe-time expectations (investigate cache staleness or consistency settings).", n)
}
