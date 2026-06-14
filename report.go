package main

// report.go writes machine-readable results plus human-readable Markdown and
// HTML findings with the measured numbers filled in.

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
	GeneratedAt       time.Time                    `json:"generated_at"`
	ToolVersion       string                       `json:"tool_version"`
	APIURL            string                       `json:"api_url"`
	Endpoint          string                       `json:"endpoint"`
	Transport         string                       `json:"transport,omitempty"` // measured-phase wire protocol when non-default; "" = http
	Consistency       string                       `json:"consistency"`
	Concurrency       int                          `json:"concurrency"`
	ClientID          int                          `json:"client_id,omitempty"`
	OfferedRate       int                          `json:"offered_rate"`
	Arrival           string                       `json:"arrival,omitempty"`               // fixed-rate arrival process when non-default; "" = uniform/closed-loop
	AchievedRate      float64                      `json:"achieved_rate_per_sec,omitempty"` // fixed-rate only: measured requests / measured window
	DroppedSlots      int64                        `json:"dropped_rate_slots,omitempty"`
	Warmup            string                       `json:"warmup"`
	Duration          string                       `json:"duration"`
	MeasuredWindow    string                       `json:"measured_window"` // first to last sample completion
	TupleCount        int                          `json:"tuple_count"`
	CorpusSize        int                          `json:"corpus_size"`
	CorpusDistinct    int                          `json:"corpus_distinct"`
	CorpusStats       map[string]CorpusTargetStats `json:"corpus_target_stats,omitempty"`
	TotalChecks       int64                        `json:"total_checks_incl_warmup"`
	Mismatches        int64                        `json:"result_mismatches"`
	Throughput        float64                      `json:"throughput_per_sec"`
	Overall           Stats                        `json:"overall"`
	ResponseLatency   *Stats                       `json:"response_latency,omitempty"` // fixed-rate only: intended send -> response
	Conditioned       Stats                        `json:"conditioned"`
	Unconditioned     Stats                        `json:"unconditioned"`
	Contextual        Stats                        `json:"contextual"`
	NoContextual      Stats                        `json:"without_contextual"`
	ByTarget          map[string]Stats             `json:"by_target"`
	DSQueriesByTarget map[string]float64           `json:"ds_queries_by_target,omitempty"` // probe-time mean datastore queries per check, per relation; absent unless probe.attribute_ds_queries ran
	ErrorsByClass     map[string]int64             `json:"errors_by_class,omitempty"`
	ErrorSamples      []string                     `json:"error_samples,omitempty"`
	Server            *ServerMetrics               `json:"server,omitempty"`     // diffed Prometheus view of the measured phase
	WriteRate         int                          `json:"write_rate,omitempty"` // background churn writes/sec; 0 = none
	WriteChurn        *Stats                       `json:"write_churn,omitempty"`
	ResultCounts      *CountStats                  `json:"result_counts,omitempty"` // list-objects/list-users: distribution of returned-set sizes
	Timeline          []TimelineBucket             `json:"timeline,omitempty"`      // per-bucket p50/p99/throughput over the measured window
	Sweep             []SweepStep                  `json:"sweep,omitempty"`
	SweepKneeRate     int                          `json:"sweep_knee_rate,omitempty"` // highest non-saturated, SLO-passing step; 0 = none
	SLOP99            string                       `json:"slo_p99,omitempty"`
	SeedDuration      string                       `json:"seed_duration,omitempty"`
	SeedRate          float64                      `json:"seed_tuples_per_sec,omitempty"`
	Environment       Environment                  `json:"environment"`
	ResolvedConfig    map[string]any               `json:"resolved_config,omitempty"` // post-defaults config, credentials redacted
	MismatchFile      string                       `json:"mismatch_file,omitempty"`   // written by Save when mismatches occurred
	Digests           *ReportDigests               `json:"digests,omitempty"`         // mergeable bounded-memory sketches for distributed aggregation
	MergedFrom        []MergedInput                `json:"merged_from,omitempty"`
	Interim           bool                         `json:"interim,omitempty"` // true for in-progress soak snapshots
	InterimIndex      int                          `json:"interim_index,omitempty"`
	ReportInterval    string                       `json:"report_interval,omitempty"`
	RepeatIndex       int                          `json:"repeat_index,omitempty"`
	RepeatTotal       int                          `json:"repeat_total,omitempty"`

	mismatchRecords []MismatchRecord // written to MismatchFile by Save
}

// MergedInput records one source report folded into a merged report.
type MergedInput struct {
	File        string  `json:"file"`
	ClientID    int     `json:"client_id,omitempty"`
	Concurrency int     `json:"concurrency"`
	OfferedRate int     `json:"offered_rate,omitempty"`
	Throughput  float64 `json:"throughput_per_sec"`
	Requests    int     `json:"requests"`
}

// Environment records where the client side of the measurement ran.
type Environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go_version"`
}

const toolVersion = "0.1.0"

// CountStats is the distribution of result-set sizes for list endpoints. The
// shape of this distribution is the headline finding for ListObjects/ListUsers:
// a relation that returns thousands of objects per call costs far more than one
// that returns a handful, and that cost is invisible in a Check-only view.
type CountStats struct {
	Responses int     `json:"responses"`
	Empty     int     `json:"empty"` // responses that returned zero results
	Min       int     `json:"min"`
	Mean      float64 `json:"mean"`
	P50       int     `json:"p50"`
	P90       int     `json:"p90"`
	P99       int     `json:"p99"`
	Max       int     `json:"max"`
	Total     int64   `json:"total"` // sum of all returned-set sizes
}

// summarizeCounts builds the result-size distribution from list-endpoint
// samples (ResultCount >= 0). Returns nil when no sample carried a count, so
// Check/BatchCheck runs omit the section.
func summarizeCounts(samples []Sample) *CountStats {
	counts := make([]int, 0, len(samples))
	for _, s := range samples {
		if s.Err || s.ResultCount < 0 {
			continue
		}
		counts = append(counts, s.ResultCount)
	}
	if len(counts) == 0 {
		return nil
	}
	sort.Ints(counts)
	cs := &CountStats{Responses: len(counts), Min: counts[0], Max: counts[len(counts)-1]}
	var sum int64
	for _, c := range counts {
		sum += int64(c)
		if c == 0 {
			cs.Empty++
		}
	}
	cs.Total = sum
	cs.Mean = float64(sum) / float64(len(counts))
	pct := func(p float64) int {
		idx := int(p*float64(len(counts))) - 1
		if idx < 0 {
			idx = 0
		}
		return counts[idx]
	}
	cs.P50, cs.P90, cs.P99 = pct(0.50), pct(0.90), pct(0.99)
	return cs
}

// TimelineBucket aggregates the measured samples that completed within one
// time slice of the measured window. The series exposes cache fill-in, GC
// pauses, and gradual degradation that the aggregate percentiles hide.
type TimelineBucket struct {
	Offset     string        `json:"offset"`     // human label, e.g. "t+5s"
	OffsetSec  int           `json:"offset_sec"` // bucket start, seconds from first measured sample
	Requests   int           `json:"requests"`
	Throughput float64       `json:"throughput_per_sec"`
	P50        time.Duration `json:"p50_ns"`
	P99        time.Duration `json:"p99_ns"`
	Errors     int           `json:"errors"`
}

// timelineWidth picks a "nice" bucket width so a run produces roughly a dozen
// rows regardless of duration: short smoke runs bucket by the second, hour-long
// runs by the minute, and multi-hour soaks by five-minute slices.
func timelineWidth(window time.Duration) time.Duration {
	if window <= 0 {
		return time.Second
	}
	target := window / 15
	for _, w := range []time.Duration{
		time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		15 * time.Second,
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		time.Hour,
		2 * time.Hour,
	} {
		if target <= w {
			return w
		}
	}
	return 2 * time.Hour
}

// buildTimeline buckets measured samples by completion time, anchored at the
// first measured completion, and summarizes each bucket. The last bucket may
// cover a partial slice; its throughput is still divided by the full width, so
// it can read low — acceptable for a trend view.
func buildTimeline(samples []Sample) []TimelineBucket {
	if len(samples) == 0 {
		return nil
	}
	anchor, last := samples[0].Completed, samples[0].Completed
	for _, s := range samples {
		if s.Completed.Before(anchor) {
			anchor = s.Completed
		}
		if s.Completed.After(last) {
			last = s.Completed
		}
	}
	width := timelineWidth(last.Sub(anchor))
	buckets := map[int][]Sample{}
	maxIdx := 0
	for _, s := range samples {
		idx := int(s.Completed.Sub(anchor) / width)
		buckets[idx] = append(buckets[idx], s)
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	out := make([]TimelineBucket, 0, maxIdx+1)
	for i := 0; i <= maxIdx; i++ {
		st := Summarize(buckets[i])
		sec := int((time.Duration(i) * width).Seconds())
		out = append(out, TimelineBucket{
			Offset:     fmt.Sprintf("t+%ds", sec),
			OffsetSec:  sec,
			Requests:   len(buckets[i]),
			Throughput: float64(st.Items) / width.Seconds(),
			P50:        st.P50,
			P99:        st.P99,
			Errors:     st.Errors,
		})
	}
	return out
}

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
		lst := res.loadStats()
		st := SweepStep{
			OfferedRate:     res.OfferedRate,
			DroppedSlots:    res.DroppedSlots,
			Overall:         lst.overall.Stats(),
			ResponseLatency: lst.response.Stats(),
			Mismatches:      res.Mismatches,
			Server:          res.Server,
		}
		window := res.MeasuredWindow.Seconds()
		if window > 0 {
			st.AchievedRate = float64(lst.TotalSamples()) / window
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
			k := m.key()
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
	lst := res.loadStats()
	r := &Report{
		GeneratedAt:    time.Now().UTC(),
		ToolVersion:    toolVersion,
		APIURL:         cfg.OpenFGA.APIURL,
		Endpoint:       res.Endpoint,
		Consistency:    res.Consistency,
		Concurrency:    res.Concurrency,
		ClientID:       cfg.Load.ClientID,
		OfferedRate:    res.OfferedRate,
		DroppedSlots:   res.DroppedSlots,
		Warmup:         res.Warmup.String(),
		Duration:       res.Duration.String(),
		MeasuredWindow: res.MeasuredWindow.Round(time.Millisecond).String(),
		TupleCount:     tupleCount,
		CorpusSize:     len(corpus.Entries),
		CorpusDistinct: corpus.Distinct(),
		CorpusStats:    corpus.Stats,
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
		Digests:        reportDigestsFromLoadStats(lst, res.OfferedRate > 0),

		mismatchRecords: res.MismatchRecords,
	}
	// Surface a non-default arrival process; leaving it empty for the uniform
	// default keeps closed-loop and uniform output byte-identical to before.
	if res.OfferedRate > 0 && res.Arrival == "poisson" {
		r.Arrival = res.Arrival
	}
	// Surface a non-default transport; HTTP runs stay byte-identical to before.
	if res.Transport == "grpc" {
		r.Transport = res.Transport
	}
	if seedDur > 0 {
		r.SeedDuration = seedDur.String()
		r.SeedRate = float64(tupleCount) / seedDur.Seconds()
	}
	r.Overall = lst.overall.Stats()
	r.Conditioned = lst.conditioned.Stats()
	r.Unconditioned = lst.unconditioned.Stats()
	r.Contextual = lst.contextual.Stats()
	r.NoContextual = lst.noContextual.Stats()
	for t, ss := range lst.byTarget {
		r.ByTarget[t] = ss.Stats()
	}
	// Carry probe-time per-relation DS attribution through to the report; nil
	// (the default) leaves the report JSON and per-relation table unchanged.
	r.DSQueriesByTarget = corpus.DSQueries
	// Throughput over the wall clock the samples actually spanned, not the
	// configured duration: in-flight requests complete after the deadline and
	// slow tails stretch the real window.
	window := res.MeasuredWindow.Seconds()
	if window <= 0 {
		window = res.Duration.Seconds()
	}
	if window > 0 {
		r.Throughput = float64(r.Overall.Items) / window
	}
	if res.OfferedRate > 0 {
		if window > 0 {
			r.AchievedRate = float64(lst.TotalSamples()) / window
		}
		rl := lst.response.Stats()
		r.ResponseLatency = &rl
	}
	if res.WriteRate > 0 {
		r.WriteRate = res.WriteRate
		ws := res.WriteStats
		r.WriteChurn = &ws
		if r.Digests != nil {
			wd := statsDigestFromLatencyStats(res.writeStats)
			r.Digests.WriteChurn = &wd
		}
	}
	r.ResultCounts = lst.resultCounts.Stats()
	r.Timeline = lst.timeline.Buckets()
	return r
}

func (r *Report) Save(dir string) (string, string, string, error) {
	paths, err := r.saveWithPatterns(dir, []string{"results-%s.json", "findings-%s.md", "report-%s.html"}, "mismatches-%s.json", func() ([][]byte, error) {
		data, err := json.MarshalIndent(r, "", " ")
		if err != nil {
			return nil, err
		}
		return [][]byte{data, []byte(r.Markdown()), []byte(r.HTML())}, nil
	})
	if err != nil {
		return "", "", "", err
	}
	return paths[0], paths[1], paths[2], nil
}

func (r *Report) SaveInterim(dir string) (string, string, error) {
	paths, err := r.saveWithPatterns(dir, []string{"interim-results-%s.json", "interim-findings-%s.md"}, "interim-mismatches-%s.json", func() ([][]byte, error) {
		data, err := json.MarshalIndent(r, "", " ")
		if err != nil {
			return nil, err
		}
		return [][]byte{data, []byte(r.Markdown())}, nil
	})
	if err != nil {
		return "", "", err
	}
	return paths[0], paths[1], nil
}

func (r *Report) saveWithPatterns(dir string, patterns []string, mismatchPattern string, payloads func() ([][]byte, error)) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stamp := r.GeneratedAt.Format("20060102-150405")
	baseCount := len(patterns)
	patterns = append([]string{}, patterns...)
	var mmData []byte
	if len(r.mismatchRecords) > 0 {
		var err error
		mmData, err = json.MarshalIndent(r.mismatchRecords, "", " ")
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, mismatchPattern)
	}
	artifacts, err := createArtifactSet(dir, stamp, patterns)
	if err != nil {
		return nil, err
	}
	if len(mmData) > 0 {
		r.MismatchFile = artifacts[baseCount].path // recorded in results JSON and human-readable docs below
	}
	payloadData, err := payloads()
	if err != nil {
		cleanupArtifacts(artifacts)
		return nil, err
	}
	if len(mmData) > 0 {
		payloadData = append(payloadData, mmData)
	}
	if len(payloadData) != len(artifacts) {
		cleanupArtifacts(artifacts)
		return nil, fmt.Errorf("internal report artifact mismatch: %d payloads for %d files", len(payloadData), len(artifacts))
	}
	if err := writeArtifacts(artifacts, payloadData); err != nil {
		return nil, err
	}
	paths := make([]string, baseCount)
	for i := 0; i < baseCount; i++ {
		paths[i] = artifacts[i].path
	}
	return paths, nil
}

type artifactFile struct {
	path string
	file *os.File
}

func createArtifactSet(dir, stamp string, patterns []string) ([]artifactFile, error) {
	for n := 0; ; n++ {
		suffix := stamp
		if n > 0 {
			suffix = fmt.Sprintf("%s-%d", stamp, n+1)
		}
		artifacts := make([]artifactFile, 0, len(patterns))
		collided := false
		for _, pattern := range patterns {
			path := filepath.Join(dir, fmt.Sprintf(pattern, suffix))
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if os.IsExist(err) {
				collided = true
				break
			}
			if err != nil {
				cleanupArtifacts(artifacts)
				return nil, err
			}
			artifacts = append(artifacts, artifactFile{path: path, file: f})
		}
		if collided {
			cleanupArtifacts(artifacts)
			continue
		}
		return artifacts, nil
	}
}

func writeArtifacts(artifacts []artifactFile, payloads [][]byte) error {
	for i := range artifacts {
		if _, err := artifacts[i].file.Write(payloads[i]); err != nil {
			cleanupArtifacts(artifacts)
			return err
		}
		if err := artifacts[i].file.Close(); err != nil {
			artifacts[i].file = nil
			cleanupArtifacts(artifacts)
			return err
		}
		artifacts[i].file = nil
	}
	return nil
}

func cleanupArtifacts(artifacts []artifactFile) {
	for _, artifact := range artifacts {
		if artifact.file != nil {
			artifact.file.Close()
		}
		os.Remove(artifact.path)
	}
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
	if r.Interim {
		if r.ReportInterval != "" {
			w("> Interim soak snapshot #%d. This covers the measured samples seen so far; the final report at run completion remains the authoritative whole-window result. Snapshot cadence: %s.", r.InterimIndex, r.ReportInterval)
		} else {
			w("> Interim soak snapshot #%d. This covers the measured samples seen so far; the final report at run completion remains the authoritative whole-window result.", r.InterimIndex)
		}
		w("")
	}
	w("New to these terms? Jump to the [How to read this](#how-to-read-this) section at the bottom for a per-column legend.")
	w("")
	w("## Test configuration")
	w("")
	w("*What this run looked like — the inputs that shape every number below. If you're comparing two runs, these are the rows that must match for the comparison to be apples-to-apples.*")
	w("")
	w("| Parameter | Value |")
	w("|---|---|")
	w("| Endpoint | %s |", r.Endpoint)
	if r.Transport == "grpc" {
		w("| Transport | gRPC |")
	}
	w("| Consistency | %s |", r.Consistency)
	w("| Concurrency | %d workers |", r.Concurrency)
	if len(r.MergedFrom) > 0 {
		w("| Load clients | %d merged reports |", len(r.MergedFrom))
	} else if r.ClientID > 0 {
		w("| Client ID | %d |", r.ClientID)
	}
	if r.RepeatTotal > 1 {
		w("| Repeat | %d of %d |", r.RepeatIndex, r.RepeatTotal)
	}
	if r.OfferedRate > 0 {
		if r.Arrival == "poisson" {
			w("| Offered rate | %d req/s (poisson arrival) |", r.OfferedRate)
		} else {
			w("| Offered rate | %d req/s |", r.OfferedRate)
		}
	} else {
		w("| Offered rate | closed loop |")
	}
	if r.WriteRate > 0 {
		w("| Background churn | %d tuple writes/sec |", r.WriteRate)
	}
	w("| Warmup / measured | %s / %s (actual window %s) |", r.Warmup, r.Duration, r.MeasuredWindow)
	w("| Seeded tuples | %d |", r.TupleCount)
	w("| Check corpus | %d entries (%d distinct checks) |", r.CorpusSize, r.CorpusDistinct)
	w("| Client | %s, %d CPU |", r.Environment.OS+"/"+r.Environment.Arch, r.Environment.CPUs)
	w("")
	w("## Summary")
	w("")
	w("%s", r.summary())
	w("")
	if suggestions := r.suggestions(); len(suggestions) > 0 {
		w("## Suggestions")
		w("")
		w("*Conservative next-run ideas based on common benchmarking traps. Treat them as prompts, not prescriptions.*")
		w("")
		for _, s := range suggestions {
			w("- %s", s)
		}
		w("")
	}
	w("## Headline results")
	w("")
	w("*Throughput and latency over the measured window. The Population column slices the same set of requests different ways: \"All checks\" is everything; the CEL/contextual rows split out paths that touched a CEL condition or carried request-scoped tuples. Compare populations of similar graph depth for a clean read.*")
	w("")
	if len(r.MergedFrom) > 0 {
		w("This report merges %d client-side result files. Concurrency, offered rate, achieved rate, throughput, errors, and mismatches are summed; latency percentiles are merged from each file's digest sketches.", len(r.MergedFrom))
		w("")
	}
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
	w("Sustained throughput was %.0f %s/sec over the %s measured window, with %d errors out of %d measured requests. %s",
		r.Throughput, endpointNoun(r.Endpoint), r.MeasuredWindow, r.Overall.Errors, r.Overall.Count+r.Overall.Errors, mismatchNote)
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
	if r.WriteChurn != nil {
		row("Background tuple writes", *r.WriteChurn)
	}
	w("")
	if r.WriteChurn != nil {
		w("All check populations above were measured while %d tuple writes/sec of background churn ran against the store, so they include any cache-invalidation cost that write traffic imposes. The \"background tuple writes\" row is the latency of those write/delete calls themselves (%d errors).",
			r.WriteRate, r.WriteChurn.Errors)
		w("")
	}
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
		w("*One row per offered rate. Read down the Achieved column: as long as it tracks Offered, the server is keeping up. Once Achieved plateaus and Response p99 starts climbing, you've passed the knee. The marked row is the highest step that still kept up.*")
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
	if r.ResultCounts != nil {
		c := r.ResultCounts
		w("## Result-set sizes")
		w("")
		w("*For `%s`, the size of the returned set is the headline cost driver — a relation that returns thousands of items per call is far more expensive than one returning a handful, and that cost is invisible in a Check-only view. Pair these with the latency table above: high latency on a relation with large result sets is expected; high latency with small sets points at a deep resolution path.*", r.Endpoint)
		w("")
		w("| Metric | Value |")
		w("|---|---|")
		w("| Responses measured | %d |", c.Responses)
		w("| Results per response | mean %.1f, p50 %d, p90 %d, p99 %d, max %d |", c.Mean, c.P50, c.P90, c.P99, c.Max)
		w("| Empty responses | %d (%.1f%%) |", c.Empty, 100*float64(c.Empty)/float64(c.Responses))
		w("| Total results returned | %d |", c.Total)
		w("")
		if cfg := r.Consistency; cfg != "" && r.Mismatches > 0 {
			w("Note: result-set verification is a spot-check (each entry's own object/user should appear in its own listing) and can flag false mismatches when a listing is truncated by OpenFGA's result cap. Treat the mismatch count for list endpoints as advisory.")
			w("")
		}
	}
	if len(r.Timeline) >= 2 {
		w("## Latency over time")
		w("")
		w("*The measured window sliced into time buckets. Aggregate percentiles hide *when* latency was bad; this catches cache fill-in (early buckets slow, later ones fast), GC pauses or compaction (one bucket spikes), and gradual degradation (p99 trending up). The bar tracks p99 relative to the worst bucket.*")
		w("")
		w("| Time | Requests | Throughput/s | p50 | p99 | p99 trend | Errors |")
		w("|---|---|---|---|---|---|---|")
		var maxP99 time.Duration
		for _, tb := range r.Timeline {
			if tb.P99 > maxP99 {
				maxP99 = tb.P99
			}
		}
		for _, tb := range r.Timeline {
			bar := ""
			if maxP99 > 0 {
				n := int(float64(tb.P99) / float64(maxP99) * 20)
				bar = strings.Repeat("█", n)
			}
			w("| %s | %d | %.0f | %s | %s | %s | %d |",
				tb.Offset, tb.Requests, tb.Throughput, ms(tb.P50), ms(tb.P99), bar, tb.Errors)
		}
		w("")
	}
	if r.Endpoint == "batch-check" {
		w("## Batch breakdown")
		w("")
		w("*Batch-check latency is measured per HTTP batch. The `batch` row can mix several target relations, so use it as a batch population view rather than a per-relation diagnostic.*")
	} else {
		w("## Per-relation breakdown")
		w("")
		w("*Latency split out by relation. This is the cleanest place to ask \"is one specific relation hot?\" — populations above mix relations of different graph depth, but here every row is a single relation. A relation with much higher p99 than its peers usually means a deeper or denser resolution path; check the model.*")
	}
	w("")
	label := "Relation"
	if r.Endpoint == "batch-check" {
		label = "Population"
	}
	// The DS column attributes per-relation datastore cost; it only makes sense
	// per relation, so it is omitted for batch-check (whose rows mix relations)
	// and whenever the attribution pass did not run.
	dsCol := r.Endpoint != "batch-check" && len(r.DSQueriesByTarget) > 0
	if dsCol {
		w("| %s | Requests | Errors | Mean | p50 | p95 | p99 | DS queries/check (probe) |", label)
		w("|---|---|---|---|---|---|---|---|")
	} else {
		w("| %s | Requests | Errors | Mean | p50 | p95 | p99 |", label)
		w("|---|---|---|---|---|---|---|")
	}
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
		if dsCol {
			dsq := "—"
			if v, ok := r.DSQueriesByTarget[t]; ok {
				dsq = fmt.Sprintf("%.1f", v)
			}
			w("| %s | %d | %d | %s | %s | %s | %s | %s |", t, s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99), dsq)
		} else {
			w("| %s | %d | %d | %s | %s | %s | %s |", t, s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99))
		}
	}
	w("")
	if dsCol {
		w("The \"DS queries/check (probe)\" column is the mean number of datastore queries OpenFGA performed per check for that relation, measured at probe time over a small distinct batch at HIGHER_CONSISTENCY, one relation at a time. It is the per-relation capacity signal: a deep tuple-to-userset relation costs many datastore reads where a direct relation costs a few. Values are best-effort histogram diffs and reflect probe-time checks, not the measured load phase.")
		w("")
	}
	if len(r.ErrorsByClass) > 0 {
		w("## Errors")
		w("")
		w("*Failed requests grouped by class. Timeouts and 5xx point at server- or datastore-side trouble (look at the server-side view, or lower offered rate). 4xx and decode errors point at fgaperf or config (mismatched model, malformed contextual tuples). Connection errors usually mean the server restarted mid-run. The `batch-item` class counts batch-check calls whose HTTP round trip succeeded but that carried at least one item-level error; their service latency is still included in the percentiles above, since the request itself completed.*")
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
		w("*OpenFGA's own metrics for the measured phase. The client-side numbers above include HTTP and JSON overhead; these don't. Use them to separate \"the server is slow\" from \"the network/serialization is slow\", and to size the datastore by datastore queries per request. On a shared OpenFGA deployment, these Prometheus counters may include unrelated traffic unless the server exposes labels you can isolate.*")
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
		w("*A throwaway baseline for tuple writes. This is the bulk-seed path (large transactional batches), so it's faster than what a per-request Write would see. Treat it as a sanity check on the datastore's write headroom, not as a write-latency benchmark.*")
		w("")
		w("Seeding %d tuples took %s, a sustained write rate of %.0f tuples/sec using transactional Write calls.",
			r.TupleCount, r.SeedDuration, r.SeedRate)
		w("")
	}
	w("## How to read this")
	w("")
	w("Reference for the columns and terms used in this document. The README's Glossary covers the same terms with links to upstream OpenFGA documentation.")
	w("")
	w("**Latency percentiles** — `p50` (median), `p90`, `p95`, `p99` are the latency values that fraction of requests came in under. `p99 = 8.0` means 99%% of requests finished in 8 ms or less, but 1%% (the tail) took longer. Tail latency drives user-visible pain; mean is rarely the right number to optimize.")
	w("")
	w("**Service latency vs response latency** — service latency is measured from \"request leaves the client\" to \"response arrives\" — what `curl` would see. Response latency is measured from each request's *scheduled* send time, so when the server falls behind and requests queue waiting for a free worker, that queueing time shows up in response latency but not in service latency. Service latency understates pain under saturation; response latency is what your real callers feel.")
	w("")
	w("**Offered rate vs achieved rate** — offered is what the load generator tried to send (set by `load.rate`). Achieved is what the server actually processed. Achieved < offered means the server fell behind; the gap shows up as dropped rate slots (a tick fired but every worker was still busy) and as rising response-latency p99.")
	w("")
	w("**Throughput** — completed requests per second over the measured window. The Mismatches count is responses whose allowed/denied differs from probe-time ground truth — usually cache staleness, sometimes a real bug. The Errors count covers timeouts, 5xx, decode failures, etc.")
	w("")
	w("**Population slices.** \"All checks\" is every measured Check or BatchCheck. \"CEL-conditioned paths\" are checks whose resolution can evaluate a CEL condition somewhere in the graph (computed statically from the model — fgaperf doesn't trace per request). \"With contextual tuples\" are checks where `contextual.attach_probability` won and the request carried contextual tuples. \"Background tuple writes\" is the churn rate's Write/Delete latency, only present when `load.write_rate > 0`.")
	w("")
	w("**Per-relation table.** \"Requests\" is sample count for that relation in the measured window; \"Errors\" counts failures attributed to checks of that relation. Compare relations of similar graph depth — a deeper relation with higher latency may be entirely expected. The \"DS queries/check (probe)\" column appears only when `probe.attribute_ds_queries` ran with a metrics endpoint: it reports how many datastore reads each relation's check costs, attributed one relation at a time at probe time — the sharpest signal for spotting an expensive rewrite.")
	w("")
	w("**Latency over time.** The measured window split into equal time buckets (the width adapts so any run is ~12 rows). \"Throughput/s\" divides each bucket's completed items by the bucket width. Read it as a trend: early buckets slower than later ones is cache warming; a single bucket spiking is a GC pause or datastore compaction; p99 trending upward across buckets is the server falling behind. The last bucket may be partial and read low.")
	w("")
	w("**Rate sweep.** \"DS queries/req\" is the server-reported mean datastore queries per Check at that offered rate; it rises sharply once OpenFGA starts spending most of its time on the database. The knee is the highest offered step that kept up (Achieved ≥ 98%% of Offered) and, if `load.slo_p99` was set, also stayed under that SLO.")
	w("")
	w("**Saturation knee** — the highest sustained rate. Past it, achieved rate plateaus and response-latency p99 climbs. Useful for capacity planning: the knee, minus headroom, is what you can safely send.")
	w("")
	w("**Server-side view.** Diffed from OpenFGA's Prometheus histograms over the measured phase, so percentiles are bucket-estimated and slightly coarser than client-side. \"Datastore queries per request\" is the most portable capacity metric — independent of network and JSON overhead, so you can use it to size the database without identical client placement. On shared servers, confirm the scraped metrics are not mixed with unrelated traffic.")
	w("")
	w("## Caveats and interpretation")
	w("")
	if r.CorpusSize > 0 && r.CorpusDistinct > 0 {
		dup := float64(r.CorpusSize) / float64(r.CorpusDistinct)
		w("The corpus replays %d distinct checks across %d entries (%.1fx average duplication). Duplication inflates server-side cache hit rates relative to production traffic; if it is high, lower probe.allowed_ratio pressure (or raise probe.samples_per_target) and rerun.", r.CorpusDistinct, r.CorpusSize, dup)
		w("")
	}
	w("Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.")
	w("")
	w("For the measurement pitfalls behind these caveats — closed-loop vs fixed-rate, coordinated omission, warmup and cache fill-in, corpus uniqueness, and why probing and load can legitimately disagree — see the [benchmarking methodology](https://github.com/joshuapsteele/fgaperf/blob/main/docs/methodology.md) page.")
	return b.String()
}

// endpointNoun names the unit of work for a given endpoint, so headline
// throughput reads correctly for list endpoints (not "checks/sec").
func endpointNoun(endpoint string) string {
	switch endpoint {
	case "list-objects":
		return "list-objects calls"
	case "list-users":
		return "list-users calls"
	default:
		return "checks"
	}
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

func (r *Report) summary() string {
	var parts []string
	if len(r.Sweep) > 0 {
		if r.SweepKneeRate > 0 {
			parts = append(parts, fmt.Sprintf("Rate sweep found a sustained knee at %d req/s.", r.SweepKneeRate))
		} else {
			parts = append(parts, "Rate sweep saturated at every offered rate.")
		}
	} else {
		parts = append(parts, fmt.Sprintf("Sustained %.0f %s/sec over %s.", r.Throughput, endpointNoun(r.Endpoint), r.MeasuredWindow))
	}
	parts = append(parts, fmt.Sprintf("Client-side p99 was %s ms.", ms(r.Overall.P99)))
	if r.Server != nil && r.Server.DatastoreQueryCount.Count > 0 {
		parts = append(parts, fmt.Sprintf("OpenFGA reported %.2f datastore queries/request.", r.Server.DatastoreQueryCount.Mean))
	}
	if r.Mismatches == 0 {
		parts = append(parts, "Verified responses had zero mismatches.")
	} else {
		parts = append(parts, fmt.Sprintf("Verified responses had %d mismatches.", r.Mismatches))
	}
	if r.Overall.Errors > 0 {
		parts = append(parts, fmt.Sprintf("%d measured requests errored.", r.Overall.Errors))
	}
	if r.OfferedRate > 0 && r.AchievedRate < 0.98*float64(r.OfferedRate) {
		parts = append(parts, fmt.Sprintf("The server did not keep up with the %d req/s offered rate.", r.OfferedRate))
	}
	if r.WriteRate > 0 {
		parts = append(parts, fmt.Sprintf("Background churn ran at %d writes/sec.", r.WriteRate))
	}
	return strings.Join(parts, " ")
}

func (r *Report) suggestions() []string {
	var out []string
	for target, st := range r.CorpusStats {
		if st.Distinct > 0 && st.Total > 2*st.Distinct {
			out = append(out, fmt.Sprintf("Target `%s` has %.1fx corpus duplication; consider raising `probe.samples_per_target` or using `probe.allowed_ratio: -1` to keep its natural mix.", target, float64(st.Total)/float64(st.Distinct)))
		}
	}
	if len(r.Sweep) == 0 && r.OfferedRate > 0 && r.AchievedRate < 0.98*float64(r.OfferedRate) {
		out = append(out, fmt.Sprintf("This fixed-rate run saturated at %.0f/%d req/s; consider a sweep across rates below %d to find the knee cleanly.", r.AchievedRate, r.OfferedRate, r.OfferedRate))
	}
	if r.Mismatches > 0 && r.Consistency == "MINIMIZE_LATENCY" && r.WriteRate > 0 {
		out = append(out, "`MINIMIZE_LATENCY` plus write churn can legitimately show stale reads; if fresh reads matter, rerun with `load.consistency: HIGHER_CONSISTENCY`.")
	}
	if r.Server != nil && r.Server.RequestDuration.P99 > 0 {
		clientP99 := float64(r.Overall.P99) / float64(time.Millisecond)
		serverP99 := r.Server.RequestDuration.P99
		if clientP99 > 5 && clientP99 > 3*serverP99 {
			out = append(out, fmt.Sprintf("Client p99 (%.2f ms) is much higher than server-side p99 (%.2f ms); check client/server co-location, network latency, and JSON/HTTP overhead.", clientP99, serverP99))
		}
	}
	if v, ok := resolvedFloat(r.ResolvedConfig, "probe", "cohort_bias"); ok && v < 0.5 {
		out = append(out, "`probe.cohort_bias` is below 0.5; on models with intersections or tuple-to-userset paths, consider raising it so the corpus includes representative allowed checks.")
	}
	return out
}

func resolvedFloat(m map[string]any, path ...string) (float64, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = mm[p]
		if !ok {
			return 0, false
		}
	}
	switch v := cur.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	}
	return 0, false
}
