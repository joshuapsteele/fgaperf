package main

// compare.go renders two results JSON files side by side: overall and
// per-relation percentiles with deltas, the server-side view when both runs
// have one, and the config differences pulled from the embedded resolved
// configs. The tool's natural use is comparative — consistency modes, cache
// settings, model variants, versions — and this makes the comparison a
// first-class artifact instead of two browser tabs.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func LoadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if r.Overall.Count == 0 && len(r.ByTarget) == 0 {
		return nil, fmt.Errorf("%s does not look like a fgaperf results file", path)
	}
	return &r, nil
}

type reportSet struct {
	Paths   []string
	Reports []*Report
}

func loadReportSet(paths []string) (reportSet, error) {
	if len(paths) == 0 {
		return reportSet{}, fmt.Errorf("compare needs at least one results file on each side")
	}
	set := reportSet{Paths: append([]string{}, paths...), Reports: make([]*Report, 0, len(paths))}
	for _, path := range paths {
		r, err := LoadReport(path)
		if err != nil {
			return reportSet{}, err
		}
		set.Reports = append(set.Reports, r)
	}
	return set, nil
}

func (s reportSet) first() *Report {
	if len(s.Reports) == 0 {
		return nil
	}
	return s.Reports[0]
}

func reportSetCell(paths []string) string {
	bases := make([]string, 0, len(paths))
	for _, p := range paths {
		bases = append(bases, filepath.Base(p))
	}
	if len(bases) <= 3 {
		return strings.Join(bases, ", ")
	}
	return fmt.Sprintf("%d files: %s, ...", len(bases), strings.Join(bases[:3], ", "))
}

func parseCompareArgs(args []string) ([]string, []string, error) {
	sep := -1
	for i, arg := range args {
		if arg == ":" {
			if sep >= 0 {
				return nil, nil, fmt.Errorf("usage: fgaperf compare <results-a.json> <results-b.json>\n   or: fgaperf compare <a-results...> : <b-results...>")
			}
			sep = i
		}
	}
	if sep >= 0 {
		a := expandCompareSpecs(args[:sep])
		b := expandCompareSpecs(args[sep+1:])
		if len(a) == 0 || len(b) == 0 {
			return nil, nil, fmt.Errorf("usage: fgaperf compare <a-results...> : <b-results...>")
		}
		return a, b, nil
	}
	if len(args) != 2 {
		return nil, nil, fmt.Errorf("usage: fgaperf compare <results-a.json> <results-b.json>\n   or: fgaperf compare <a-results...> : <b-results...>")
	}
	a := expandCompareSpecs(args[:1])
	b := expandCompareSpecs(args[1:])
	if len(a) == 0 || len(b) == 0 {
		return nil, nil, fmt.Errorf("usage: fgaperf compare <results-a.json> <results-b.json>")
	}
	return a, b, nil
}

func expandCompareSpecs(specs []string) []string {
	var out []string
	for _, spec := range specs {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// comparabilityCaveats returns loud warnings for differences that make a
// comparison misleading rather than merely interesting.
func comparabilityCaveats(a, b *Report) []string {
	var out []string
	if a.Endpoint != b.Endpoint {
		out = append(out, fmt.Sprintf("endpoint differs (%s vs %s); the populations are not comparable", a.Endpoint, b.Endpoint))
	} else if endpointMixKey(a) != endpointMixKey(b) {
		// Same label ("mixed") but a different blend: the per-endpoint mix and the
		// blended headline are not comparable even though the labels match.
		out = append(out, fmt.Sprintf("endpoint blend differs (%s vs %s); the endpoint mix is not comparable", endpointMixSentence(a.EndpointMix), endpointMixSentence(b.EndpointMix)))
	}
	if a.CorpusSize != b.CorpusSize {
		out = append(out, fmt.Sprintf("corpus size differs (%d vs %d entries); per-relation populations may not line up", a.CorpusSize, b.CorpusSize))
	}
	if a.Duration != b.Duration {
		out = append(out, fmt.Sprintf("measured duration differs (%s vs %s); percentile stability differs between the runs", a.Duration, b.Duration))
	}
	if a.Warmup != b.Warmup {
		out = append(out, fmt.Sprintf("warmup differs (%s vs %s); cache and connection state may differ at measurement start", a.Warmup, b.Warmup))
	}
	if a.Concurrency != b.Concurrency {
		out = append(out, fmt.Sprintf("concurrency differs (%d vs %d workers); closed-loop throughput is not comparable", a.Concurrency, b.Concurrency))
	}
	if a.OfferedRate != b.OfferedRate {
		out = append(out, fmt.Sprintf("offered rate differs (%d vs %d req/s); latency and saturation are not directly comparable", a.OfferedRate, b.OfferedRate))
	}
	if a.WriteRate != b.WriteRate {
		out = append(out, fmt.Sprintf("write churn differs (%d vs %d writes/sec); cache invalidation pressure differs", a.WriteRate, b.WriteRate))
	}
	if (len(a.Sweep) > 0) != (len(b.Sweep) > 0) {
		out = append(out, "sweep mode differs; one report is a multi-rate sweep and the other is a single measured run")
	}
	if a.ToolVersion != b.ToolVersion {
		out = append(out, fmt.Sprintf("tool version differs (%s vs %s)", a.ToolVersion, b.ToolVersion))
	}
	return out
}

// diffConfigs walks two resolved-config maps and returns "path: a -> b" lines
// for every leaf that differs.
func diffConfigs(a, b map[string]any) []string {
	var out []string
	diffValues("", a, b, &out)
	sort.Strings(out)
	return out
}

func diffValues(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diffValues(p, am[k], bm[k], out)
		}
		return
	}
	if fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) {
		*out = append(*out, fmt.Sprintf("%s: %v -> %v", path, render(a), render(b)))
	}
}

func render(v any) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprintf("%v", v)
}

func deltaCell(a, b time.Duration) string {
	d := float64(b-a) / float64(time.Millisecond)
	if a == 0 {
		return fmt.Sprintf("%+.2f ms", d)
	}
	return fmt.Sprintf("%+.2f ms (%+.1f%%)", d, 100*float64(b-a)/float64(a))
}

func durationCell(st Stats, d time.Duration) string {
	if st.Count == 0 {
		return "—"
	}
	return ms(d)
}

func durationDeltaCell(a Stats, av time.Duration, b Stats, bv time.Duration) string {
	if a.Count == 0 || b.Count == 0 {
		return "—"
	}
	return deltaCell(av, bv)
}

type seriesStats struct {
	N      int
	Mean   float64
	StdDev float64
}

func summarizeSeries(vals []float64) seriesStats {
	out := seriesStats{N: len(vals)}
	if len(vals) == 0 {
		return out
	}
	for _, v := range vals {
		out.Mean += v
	}
	out.Mean /= float64(len(vals))
	if len(vals) == 1 {
		return out
	}
	var sumSquares float64
	for _, v := range vals {
		d := v - out.Mean
		sumSquares += d * d
	}
	out.StdDev = math.Sqrt(sumSquares / float64(len(vals)-1))
	return out
}

func reportSeries(set reportSet, value func(*Report) float64) []float64 {
	vals := make([]float64, 0, len(set.Reports))
	for _, r := range set.Reports {
		vals = append(vals, value(r))
	}
	return vals
}

func serverSeries(set reportSet, value func(*ServerMetrics) (float64, bool)) []float64 {
	vals := make([]float64, 0, len(set.Reports))
	for _, r := range set.Reports {
		if r.Server == nil {
			continue
		}
		if v, ok := value(r.Server); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

func targetDurationSeries(set reportSet, target string, value func(Stats) time.Duration) []float64 {
	vals := make([]float64, 0, len(set.Reports))
	for _, r := range set.Reports {
		if st, ok := r.ByTarget[target]; ok {
			if st.Count == 0 {
				continue
			}
			vals = append(vals, durationMillis(value(st)))
		}
	}
	return vals
}

func targetRequestErrorTotals(set reportSet, target string) (requests int, errors int) {
	for _, r := range set.Reports {
		st, ok := r.ByTarget[target]
		if !ok {
			continue
		}
		requests += st.Count + st.Errors
		errors += st.Errors
	}
	return requests, errors
}

func durationMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

func formatSeries(st seriesStats, unit string, precision int) string {
	if st.N == 0 {
		return "—"
	}
	if st.N == 1 {
		return fmt.Sprintf("%.*f%s", precision, st.Mean, unit)
	}
	return fmt.Sprintf("%.*f +/- %.*f%s", precision, st.Mean, precision, st.StdDev, unit)
}

func deltaSeriesCell(a, b seriesStats, unit string, precision int, percent bool) string {
	if a.N == 0 || b.N == 0 {
		return "—"
	}
	delta := b.Mean - a.Mean
	out := fmt.Sprintf("%+.*f%s", precision, delta, unit)
	if percent && a.Mean != 0 {
		out += fmt.Sprintf(" (%+.1f%%)", 100*delta/a.Mean)
	}
	return out
}

func significanceLabel(a, b seriesStats) string {
	if a.N == 0 || b.N == 0 {
		return "—"
	}
	if a.N < 2 || b.N < 2 {
		return "insufficient repeats"
	}
	delta := b.Mean - a.Mean
	va := a.StdDev * a.StdDev
	vb := b.StdDev * b.StdDev
	se2 := va/float64(a.N) + vb/float64(b.N)
	if se2 == 0 {
		if delta == 0 {
			return "within noise"
		}
		return "significant"
	}
	aTerm := va / float64(a.N)
	bTerm := vb / float64(b.N)
	denom := 0.0
	if a.N > 1 {
		denom += aTerm * aTerm / float64(a.N-1)
	}
	if b.N > 1 {
		denom += bTerm * bTerm / float64(b.N-1)
	}
	df := math.Inf(1)
	if denom > 0 {
		df = (se2 * se2) / denom
	}
	if math.Abs(delta) > tCritical95(df)*math.Sqrt(se2) {
		return "significant"
	}
	return "within noise"
}

func tCritical95(df float64) float64 {
	table := []struct {
		df float64
		t  float64
	}{
		{1, 12.706}, {2, 4.303}, {3, 3.182}, {4, 2.776}, {5, 2.571},
		{6, 2.447}, {7, 2.365}, {8, 2.306}, {9, 2.262}, {10, 2.228},
		{11, 2.201}, {12, 2.179}, {13, 2.160}, {14, 2.145}, {15, 2.131},
		{16, 2.120}, {17, 2.110}, {18, 2.101}, {19, 2.093}, {20, 2.086},
		{24, 2.064}, {30, 2.042}, {40, 2.021}, {60, 2.000}, {120, 1.980},
	}
	if math.IsInf(df, 1) || df > table[len(table)-1].df {
		return 1.960
	}
	if df <= table[0].df {
		return table[0].t
	}
	for i := 1; i < len(table); i++ {
		if df <= table[i].df {
			prev := table[i-1]
			next := table[i]
			frac := (df - prev.df) / (next.df - prev.df)
			return prev.t + frac*(next.t-prev.t)
		}
	}
	return 1.960
}

func writeRepeatedMetricRow(w func(string, ...any), name string, valsA, valsB []float64, unit string, precision int, percent bool) {
	a := summarizeSeries(valsA)
	b := summarizeSeries(valsB)
	if a.N == 0 || b.N == 0 {
		w("| %s | %s | %s | — | unavailable |", name, formatSeries(a, unit, precision), formatSeries(b, unit, precision))
		return
	}
	w("| %s | %s | %s | %s | %s |", name,
		formatSeries(a, unit, precision),
		formatSeries(b, unit, precision),
		deltaSeriesCell(a, b, unit, precision, percent),
		significanceLabel(a, b))
}

func withinSetCaveats(label string, set reportSet) []string {
	if len(set.Reports) < 2 {
		return nil
	}
	base := set.Reports[0]
	var out []string
	for i := 1; i < len(set.Reports); i++ {
		r := set.Reports[i]
		for _, c := range comparabilityCaveats(base, r) {
			out = append(out, fmt.Sprintf("%s repeat %d (%s) differs from repeat 1: %s", label, i+1, filepath.Base(set.Paths[i]), c))
		}
		if base.ResolvedConfig != nil && r.ResolvedConfig != nil {
			diffs := diffConfigs(base.ResolvedConfig, r.ResolvedConfig)
			if len(diffs) > 0 {
				out = append(out, fmt.Sprintf("%s repeat %d (%s) resolved config differs from repeat 1 (%d keys; first: %s)", label, i+1, filepath.Base(set.Paths[i]), len(diffs), diffs[0]))
			}
		}
	}
	return out
}

func CompareMarkdown(pathA, pathB string, a, b *Report) string {
	return CompareSetMarkdown(
		reportSet{Paths: []string{pathA}, Reports: []*Report{a}},
		reportSet{Paths: []string{pathB}, Reports: []*Report{b}},
	)
}

func CompareSetMarkdown(aSet, bSet reportSet) string {
	a, b := aSet.first(), bSet.first()
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }

	w("# fgaperf comparison")
	w("")
	w("| | A | B |")
	w("|---|---|---|")
	w("| File | %s | %s |", reportSetCell(aSet.Paths), reportSetCell(bSet.Paths))
	if len(aSet.Reports) > 1 || len(bSet.Reports) > 1 {
		w("| Repeats | %d | %d |", len(aSet.Reports), len(bSet.Reports))
	}
	w("| Generated | %s | %s |", a.GeneratedAt.Format("2006-01-02 15:04 UTC"), b.GeneratedAt.Format("2006-01-02 15:04 UTC"))
	w("| Endpoint / consistency | %s / %s | %s / %s |", a.Endpoint, a.Consistency, b.Endpoint, b.Consistency)
	w("| Corpus | %d entries (%d distinct) | %d entries (%d distinct) |", a.CorpusSize, a.CorpusDistinct, b.CorpusSize, b.CorpusDistinct)
	w("")
	caveats := comparabilityCaveats(a, b)
	caveats = append(caveats, withinSetCaveats("A", aSet)...)
	caveats = append(caveats, withinSetCaveats("B", bSet)...)
	if len(caveats) > 0 {
		w("> **⚠️ These runs are not directly comparable:**")
		for _, c := range caveats {
			w("> - %s", c)
		}
		w("")
	}
	repeated := len(aSet.Reports) > 1 || len(bSet.Reports) > 1
	if repeated {
		w("Repeated cells are mean +/- sample stdev. Signal uses a two-sided Welch t-test at alpha=0.05; \"within noise\" means the mean delta did not clear observed run-to-run variance.")
		w("")
	}

	w("## Overall")
	w("")
	if repeated {
		w("| Metric | A | B | Δ (B − A) | Signal |")
		w("|---|---|---|---|---|")
		writeRepeatedMetricRow(w, "Throughput", reportSeries(aSet, func(r *Report) float64 { return r.Throughput }), reportSeries(bSet, func(r *Report) float64 { return r.Throughput }), "/s", 1, false)
		writeRepeatedMetricRow(w, "Mean", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.Mean) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.Mean) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "p50", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.P50) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.P50) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "p90", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.P90) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.P90) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "p95", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.P95) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.P95) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "p99", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.P99) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.P99) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "Max", reportSeries(aSet, func(r *Report) float64 { return durationMillis(r.Overall.Max) }), reportSeries(bSet, func(r *Report) float64 { return durationMillis(r.Overall.Max) }), " ms", 2, true)
		writeRepeatedMetricRow(w, "Errors", reportSeries(aSet, func(r *Report) float64 { return float64(r.Overall.Errors) }), reportSeries(bSet, func(r *Report) float64 { return float64(r.Overall.Errors) }), "", 1, false)
		writeRepeatedMetricRow(w, "Mismatches", reportSeries(aSet, func(r *Report) float64 { return float64(r.Mismatches) }), reportSeries(bSet, func(r *Report) float64 { return float64(r.Mismatches) }), "", 1, false)
	} else {
		w("| Metric | A | B | Δ (B − A) |")
		w("|---|---|---|---|")
		w("| Throughput | %.0f/s | %.0f/s | %+.0f/s |", a.Throughput, b.Throughput, b.Throughput-a.Throughput)
		row := func(name string, av, bv time.Duration) {
			w("| %s | %s ms | %s ms | %s |", name, ms(av), ms(bv), deltaCell(av, bv))
		}
		row("Mean", a.Overall.Mean, b.Overall.Mean)
		row("p50", a.Overall.P50, b.Overall.P50)
		row("p90", a.Overall.P90, b.Overall.P90)
		row("p95", a.Overall.P95, b.Overall.P95)
		row("p99", a.Overall.P99, b.Overall.P99)
		row("Max", a.Overall.Max, b.Overall.Max)
		w("| Errors | %d | %d | %+d |", a.Overall.Errors, b.Overall.Errors, b.Overall.Errors-a.Overall.Errors)
		w("| Mismatches | %d | %d | %+d |", a.Mismatches, b.Mismatches, b.Mismatches-a.Mismatches)
	}
	w("")

	dsA := serverSeries(aSet, func(s *ServerMetrics) (float64, bool) {
		return s.DatastoreQueryCount.Mean, s.DatastoreQueryCount.Count > 0
	})
	dsB := serverSeries(bSet, func(s *ServerMetrics) (float64, bool) {
		return s.DatastoreQueryCount.Mean, s.DatastoreQueryCount.Count > 0
	})
	if len(dsA) > 0 && len(dsB) > 0 {
		w("## Server-side view")
		w("")
		if repeated {
			w("| Metric | A | B | Δ (B − A) | Signal |")
			w("|---|---|---|---|---|")
			writeRepeatedMetricRow(w, "Datastore queries per request", dsA, dsB, "", 2, false)
			writeRepeatedMetricRow(w, "Server-side p99", serverSeries(aSet, func(s *ServerMetrics) (float64, bool) {
				return s.RequestDuration.P99, s.RequestDuration.Count > 0
			}), serverSeries(bSet, func(s *ServerMetrics) (float64, bool) {
				return s.RequestDuration.P99, s.RequestDuration.Count > 0
			}), " ms", 2, true)
			writeRepeatedMetricRow(w, "Check cache hit rate", serverSeries(aSet, func(s *ServerMetrics) (float64, bool) {
				if s.CheckCacheTotal == 0 {
					return 0, false
				}
				return 100 * s.CheckCacheHits / s.CheckCacheTotal, true
			}), serverSeries(bSet, func(s *ServerMetrics) (float64, bool) {
				if s.CheckCacheTotal == 0 {
					return 0, false
				}
				return 100 * s.CheckCacheHits / s.CheckCacheTotal, true
			}), "%", 1, false)
		} else {
			w("| Metric | A | B | Δ (B − A) |")
			w("|---|---|---|---|")
			w("| Datastore queries per request | %.2f | %.2f | %+.2f |",
				a.Server.DatastoreQueryCount.Mean, b.Server.DatastoreQueryCount.Mean,
				b.Server.DatastoreQueryCount.Mean-a.Server.DatastoreQueryCount.Mean)
			w("| Server-side p99 | %.2f ms | %.2f ms | %+.2f ms |",
				a.Server.RequestDuration.P99, b.Server.RequestDuration.P99,
				b.Server.RequestDuration.P99-a.Server.RequestDuration.P99)
			cacheRate := func(s *ServerMetrics) string {
				if s.CheckCacheTotal == 0 {
					return "—"
				}
				return fmt.Sprintf("%.1f%%", 100*s.CheckCacheHits/s.CheckCacheTotal)
			}
			w("| Check cache hit rate | %s | %s | |", cacheRate(a.Server), cacheRate(b.Server))
		}
		w("")
	}

	w("## Per-relation p50 / p99")
	w("")
	if repeated {
		w("| Relation | A req | A err | B req | B err | A p50 | B p50 | Δ p50 | p50 signal | A p99 | B p99 | Δ p99 | p99 signal |")
		w("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	} else {
		w("| Relation | A req | A err | B req | B err | A p50 | B p50 | Δ p50 | A p99 | B p99 | Δ p99 |")
		w("|---|---|---|---|---|---|---|---|---|---|---|")
	}
	targets := map[string]bool{}
	for _, r := range aSet.Reports {
		for t := range r.ByTarget {
			targets[t] = true
		}
	}
	for _, r := range bSet.Reports {
		for t := range r.ByTarget {
			targets[t] = true
		}
	}
	names := make([]string, 0, len(targets))
	for t := range targets {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		if repeated {
			aReq, aErr := targetRequestErrorTotals(aSet, t)
			bReq, bErr := targetRequestErrorTotals(bSet, t)
			aP50 := targetDurationSeries(aSet, t, func(s Stats) time.Duration { return s.P50 })
			bP50 := targetDurationSeries(bSet, t, func(s Stats) time.Duration { return s.P50 })
			aP99 := targetDurationSeries(aSet, t, func(s Stats) time.Duration { return s.P99 })
			bP99 := targetDurationSeries(bSet, t, func(s Stats) time.Duration { return s.P99 })
			a50, b50 := summarizeSeries(aP50), summarizeSeries(bP50)
			a99, b99 := summarizeSeries(aP99), summarizeSeries(bP99)
			w("| %s | %d | %d | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s |", t,
				aReq, aErr, bReq, bErr,
				formatSeries(a50, " ms", 2), formatSeries(b50, " ms", 2), deltaSeriesCell(a50, b50, " ms", 2, true), significanceLabel(a50, b50),
				formatSeries(a99, " ms", 2), formatSeries(b99, " ms", 2), deltaSeriesCell(a99, b99, " ms", 2, true), significanceLabel(a99, b99))
			continue
		}
		as, aok := a.ByTarget[t]
		bs, bok := b.ByTarget[t]
		if !aok || !bok {
			side := map[bool]string{true: "A", false: "B"}[aok]
			w("| %s | _only in %s_ | | | | | | | | | |", t, side)
			continue
		}
		w("| %s | %d | %d | %d | %d | %s | %s | %s | %s | %s | %s |", t,
			as.Count+as.Errors, as.Errors, bs.Count+bs.Errors, bs.Errors,
			durationCell(as, as.P50), durationCell(bs, bs.P50), durationDeltaCell(as, as.P50, bs, bs.P50),
			durationCell(as, as.P99), durationCell(bs, bs.P99), durationDeltaCell(as, as.P99, bs, bs.P99))
	}
	w("")

	w("## Config differences")
	w("")
	switch {
	case a.ResolvedConfig == nil || b.ResolvedConfig == nil:
		w("At least one results file predates embedded resolved configs; config diff unavailable.")
	default:
		diffs := diffConfigs(a.ResolvedConfig, b.ResolvedConfig)
		if len(diffs) == 0 {
			w("None — the embedded resolved configs are identical.")
		} else {
			for _, d := range diffs {
				w("- `%s`", d)
			}
		}
	}
	w("")
	return sb.String()
}

func compare(pathA, pathB, outDir string) error {
	return compareAt(pathA, pathB, outDir, time.Now().UTC())
}

func compareSets(pathsA, pathsB []string, outDir string) error {
	return compareSetsAt(pathsA, pathsB, outDir, time.Now().UTC())
}

func compareAt(pathA, pathB, outDir string, generatedAt time.Time) error {
	return compareSetsAt([]string{pathA}, []string{pathB}, outDir, generatedAt)
}

func compareSetsAt(pathsA, pathsB []string, outDir string, generatedAt time.Time) error {
	a, err := loadReportSet(pathsA)
	if err != nil {
		return err
	}
	b, err := loadReportSet(pathsB)
	if err != nil {
		return err
	}
	md := CompareSetMarkdown(a, b)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	artifacts, err := createArtifactSet(outDir, generatedAt.Format("20060102-150405"), []string{"compare-%s.md"})
	if err != nil {
		return err
	}
	if err := writeArtifacts(artifacts, [][]byte{[]byte(md)}); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", artifacts[0].path)
	return nil
}
