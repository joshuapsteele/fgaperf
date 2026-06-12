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
	Sweep           []SweepStep      `json:"sweep,omitempty"`
	SweepKneeRate   int              `json:"sweep_knee_rate,omitempty"` // highest non-saturated, SLO-passing step; 0 = none
	SLOP99          string           `json:"slo_p99,omitempty"`
	SeedDuration    string           `json:"seed_duration,omitempty"`
	SeedRate        float64          `json:"seed_tuples_per_sec,omitempty"`
	Environment     Environment      `json:"environment"`
	ResolvedConfig  map[string]any   `json:"resolved_config,omitempty"` // post-defaults config, credentials redacted
	MismatchFile    string           `json:"mismatch_file,omitempty"`   // written by Save when mismatches occurred

	mismatchRecords []MismatchRecord // written to MismatchFile by Save
}

// Environment records where the client side of the measurement ran.
type Environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go_version"`
}

const toolVersion = "0.1.0"

// SweepStep is one fixed-rate step of a sweep run.
type SweepStep struct {
	OfferedRate     int            `json:"offered_rate"`
	AchievedRate    float64        `json:"achieved_rate_per_sec"`
	DroppedSlots    int64          `json:"dropped_rate_slots"`
	Throughput      float64        `json:"throughput_per_sec"`
	Overall         Stats          `json:"overall"`
	ResponseLatency Stats          `json:"response_latency"`
	Mismatches      int64          `json:"result_mismatches"`
	Server          *ServerMetrics `json:"server,omitempty"`
	Saturated       bool           `json:"saturated"` // achieved < 98% of offered
	PassesSLO       bool           `json:"passes_slo"`
}

// BuildSweepReport assembles a report whose headline sections reflect the
// knee step — the highest offered rate the server sustained within the SLO —
// with every step's stats in Sweep. Falls back to the last step when every
// step saturated.
func BuildSweepReport(results []*LoadResult, corpus *Corpus, cfg *Config, tupleCount int, seedDur time.Duration) *Report {
	steps := make([]SweepStep, 0, len(results))
	kneeIdx := -1
	for i, res := range results {
		st := SweepStep{
			OfferedRate:     res.OfferedRate,
			DroppedSlots:    res.DroppedSlots,
			Overall:         Summarize(res.Samples),
			ResponseLatency: SummarizeResponse(res.Samples),
			Mismatches:      res.Mismatches,
			Server:          res.Server,
		}
		window := res.MeasuredWindow.Seconds()
		if window > 0 {
			st.AchievedRate = float64(len(res.Samples)) / window
			st.Throughput = float64(st.Overall.Items) / window
		}
		st.Saturated = st.AchievedRate < 0.98*float64(st.OfferedRate)
		st.PassesSLO = cfg.Load.SLOP99 == 0 || st.ResponseLatency.P99 <= cfg.Load.SLOP99
		if !st.Saturated && st.PassesSLO && (kneeIdx == -1 || st.OfferedRate > steps[kneeIdx].OfferedRate) {
			kneeIdx = i
		}
		steps = append(steps, st)
	}
	headline := kneeIdx
	if headline == -1 {
		headline = len(results) - 1
	}
	r := BuildReport(results[headline], corpus, cfg, tupleCount, seedDur)
	r.Sweep = steps
	if kneeIdx >= 0 {
		r.SweepKneeRate = steps[kneeIdx].OfferedRate
	}
	// Merge mismatch records across all steps, not just the headline one.
	seen := map[string]bool{}
	r.mismatchRecords = nil
	for _, res := range results {
		for _, m := range res.MismatchRecords {
			k := m.User + "|" + m.Relation + "|" + m.Object
			if seen[k] || len(r.mismatchRecords) >= maxMismatchRecords {
				continue
			}
			seen[k] = true
			r.mismatchRecords = append(r.mismatchRecords, m)
		}
	}
	if cfg.Load.SLOP99 > 0 {
		r.SLOP99 = cfg.Load.SLOP99.String()
	}
	return r
}

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

		mismatchRecords: res.MismatchRecords,
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
	if len(r.mismatchRecords) > 0 {
		mmPath := filepath.Join(dir, "mismatches-"+stamp+".json")
		mmData, err := json.MarshalIndent(r.mismatchRecords, "", " ")
		if err != nil {
			return "", "", err
		}
		if err := os.WriteFile(mmPath, mmData, 0o644); err != nil {
			return "", "", err
		}
		r.MismatchFile = mmPath // recorded in results JSON and findings doc below
	}
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
	if len(r.Sweep) > 0 {
		if r.SweepKneeRate > 0 {
			w("This run swept %d offered rates; this section and the per-relation table reflect the knee step at %d req/s — the highest rate the server sustained%s. The full curve is in the Rate sweep section below.",
				len(r.Sweep), r.SweepKneeRate, sloClause(r.SLOP99))
		} else {
			w("This run swept %d offered rates and **every step saturated**; this section reflects the final step. The full curve is in the Rate sweep section below.", len(r.Sweep))
		}
		w("")
	}
	mismatchNote := mismatchSentence(r.Mismatches)
	if r.MismatchFile != "" {
		mismatchNote += fmt.Sprintf(" The mismatched checks (deduplicated, capped at %d) are listed in `%s`.", maxMismatchRecords, filepath.Base(r.MismatchFile))
	}
	w("Sustained throughput was %.0f checks/sec over the %s measured window, with %d errors out of %d measured requests. %s",
		r.Throughput, r.MeasuredWindow, r.Overall.Errors, r.Overall.Count+r.Overall.Errors, mismatchNote)
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
	if len(r.Sweep) > 0 {
		w("## Rate sweep")
		w("")
		w("| Offered req/s | Achieved | Dropped slots | p50 | p95 | p99 | Response p99 | Errors | DS queries/req |")
		w("|---|---|---|---|---|---|---|---|---|")
		for _, st := range r.Sweep {
			dsq := "—"
			if st.Server != nil && st.Server.DatastoreQueryCount.Count > 0 {
				dsq = fmt.Sprintf("%.1f", st.Server.DatastoreQueryCount.Mean)
			}
			mark := ""
			if st.OfferedRate == r.SweepKneeRate && r.SweepKneeRate > 0 {
				mark = " ◀ knee"
			}
			w("| %d%s | %.0f | %d | %s | %s | %s | %s | %d | %s |",
				st.OfferedRate, mark, st.AchievedRate, st.DroppedSlots,
				ms(st.Overall.P50), ms(st.Overall.P95), ms(st.Overall.P99), ms(st.ResponseLatency.P99),
				st.Overall.Errors, dsq)
		}
		w("")
		if r.SweepKneeRate > 0 {
			w("The last step where the server kept up with the offered rate (achieved ≥ 98%% of offered%s) was **%d req/s**. Steps beyond the knee show response-latency p99 diverging from service p99 — that gap is queueing delay.",
				sloClause(r.SLOP99), r.SweepKneeRate)
		} else {
			w("**No step kept up with its offered rate%s.** Re-run with lower rates to find the knee.", sloClause(r.SLOP99))
		}
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

func sloClause(slo string) string {
	if slo == "" {
		return ""
	}
	return fmt.Sprintf(" with response-latency p99 under the %s SLO", slo)
}

func mismatchSentence(n int64) string {
	if n == 0 {
		return "All verified responses matched probe-time expectations."
	}
	return fmt.Sprintf("%d responses differed from probe-time expectations (investigate cache staleness or consistency settings).", n)
}
