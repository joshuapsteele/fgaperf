package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// mergeReports combines several digest-enabled results files into one report.
// It is intentionally an offline merge: each load generator runs the normal
// `run` command against the same store/corpus, then this command folds the
// bounded-memory sketches together.
func mergeReports(paths []string, outDir string) error {
	jsonPath, mdPath, htmlPath, err := mergeReportsAt(paths, outDir, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("merged %d reports into %s, %s, and %s\n", len(paths), jsonPath, mdPath, htmlPath)
	return nil
}

func mergeReportsAt(paths []string, outDir string, generatedAt time.Time) (string, string, string, error) {
	r, err := buildMergedReport(paths, generatedAt)
	if err != nil {
		return "", "", "", err
	}
	return r.Save(outDir)
}

func buildMergedReport(paths []string, generatedAt time.Time) (*Report, error) {
	if len(paths) < 2 {
		return nil, fmt.Errorf("merge needs at least two results JSON files")
	}
	reports := make([]*Report, 0, len(paths))
	for _, path := range paths {
		r, err := LoadReport(path)
		if err != nil {
			return nil, err
		}
		if len(r.Sweep) > 0 {
			return nil, fmt.Errorf("%s is a sweep report; merge currently supports single-rate reports only", path)
		}
		if r.Digests == nil || r.Digests.Overall.IsZero() {
			return nil, fmt.Errorf("%s does not contain mergeable digests; rerun with this fgaperf version", path)
		}
		reports = append(reports, r)
	}
	base := reports[0]
	for i := 1; i < len(reports); i++ {
		if err := checkMergeCompatible(paths[0], base, paths[i], reports[i]); err != nil {
			return nil, err
		}
	}

	stats := newLoadStats()
	var writeChurn latencyStats
	var concurrency, offeredRate int
	var dropped, totalChecks, mismatches int64
	var throughput, achieved float64
	errorsByClass := map[string]int64{}
	var errorSamples []string
	inputs := make([]MergedInput, 0, len(reports))
	for i, r := range reports {
		stats.Merge(r.Digests.loadStats())
		if r.Digests.WriteChurn != nil {
			writeChurn.Merge(r.Digests.WriteChurn.latencyStats())
		}
		concurrency += r.Concurrency
		offeredRate += r.OfferedRate
		dropped += r.DroppedSlots
		totalChecks += r.TotalChecks
		mismatches += r.Mismatches
		throughput += r.Throughput
		achieved += r.AchievedRate
		for class, n := range r.ErrorsByClass {
			errorsByClass[class] += n
		}
		for _, sample := range r.ErrorSamples {
			if len(errorSamples) >= maxErrorSamples {
				break
			}
			errorSamples = append(errorSamples, sample)
		}
		inputs = append(inputs, MergedInput{
			File:        filepath.Base(paths[i]),
			ClientID:    r.ClientID,
			Concurrency: r.Concurrency,
			OfferedRate: r.OfferedRate,
			Throughput:  r.Throughput,
			Requests:    r.Overall.Count + r.Overall.Errors,
		})
	}

	merged := &Report{
		GeneratedAt:       generatedAt,
		ToolVersion:       toolVersion,
		APIURL:            base.APIURL,
		Endpoint:          base.Endpoint,
		Consistency:       base.Consistency,
		Concurrency:       concurrency,
		OfferedRate:       offeredRate,
		DroppedSlots:      dropped,
		Warmup:            base.Warmup,
		Duration:          base.Duration,
		MeasuredWindow:    mergedWindow(stats, reports),
		TupleCount:        base.TupleCount,
		CorpusSize:        base.CorpusSize,
		CorpusDistinct:    base.CorpusDistinct,
		CorpusStats:       base.CorpusStats,
		TotalChecks:       totalChecks,
		Mismatches:        mismatches,
		Throughput:        throughput,
		EndpointMix:       base.EndpointMix,
		ByTarget:          map[string]Stats{},
		DSQueriesByTarget: base.DSQueriesByTarget,
		ErrorsByClass:     errorsByClass,
		ErrorSamples:      errorSamples,
		Environment:       Environment{OS: "merged", Arch: "multi-client"},
		ResolvedConfig:    base.ResolvedConfig,
		Digests:           reportDigestsFromLoadStats(stats, offeredRate > 0),
		MergedFrom:        inputs,
	}
	if normalizedTransport(base) == "grpc" {
		merged.Transport = "grpc"
	}
	if offeredRate > 0 {
		merged.AchievedRate = achieved
		if normalizedArrival(base) == "poisson" {
			merged.Arrival = "poisson"
		}
		rl := stats.response.Stats()
		merged.ResponseLatency = &rl
	}
	merged.Overall = stats.overall.Stats()
	merged.Conditioned = stats.conditioned.Stats()
	merged.Unconditioned = stats.unconditioned.Stats()
	merged.Contextual = stats.contextual.Stats()
	merged.NoContextual = stats.noContextual.Stats()
	for target, ss := range stats.byTarget {
		merged.ByTarget[target] = ss.Stats()
	}
	if len(stats.byEndpoint) > 1 {
		merged.ByEndpoint = map[string]Stats{}
		for endpoint, ss := range stats.byEndpoint {
			merged.ByEndpoint[endpoint] = ss.Stats()
		}
	}
	merged.ResultCounts = stats.resultCounts.Stats()
	merged.Timeline = stats.timeline.Buckets()
	if writeChurn.digest.count+writeChurn.errors > 0 {
		writeRate := 0
		for _, r := range reports {
			writeRate += r.WriteRate
		}
		merged.WriteRate = writeRate
		ws := writeChurn.Stats()
		merged.WriteChurn = &ws
		if merged.Digests != nil {
			wd := statsDigestFromLatencyStats(writeChurn)
			merged.Digests.WriteChurn = &wd
		}
	}
	return merged, nil
}

func checkMergeCompatible(pathA string, a *Report, pathB string, b *Report) error {
	pair := func(name string, av, bv any) error {
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			return fmt.Errorf("cannot merge %s with %s: %s differs (%v vs %v)", pathA, pathB, name, av, bv)
		}
		return nil
	}
	for _, c := range []struct {
		name string
		a    any
		b    any
	}{
		{"api_url", a.APIURL, b.APIURL},
		{"endpoint", a.Endpoint, b.Endpoint},
		{"endpoint_mix", endpointMixKey(a), endpointMixKey(b)},
		{"transport", normalizedTransport(a), normalizedTransport(b)},
		{"consistency", a.Consistency, b.Consistency},
		{"warmup", a.Warmup, b.Warmup},
		{"duration", a.Duration, b.Duration},
		{"tuple_count", a.TupleCount, b.TupleCount},
		{"corpus_size", a.CorpusSize, b.CorpusSize},
		{"corpus_distinct", a.CorpusDistinct, b.CorpusDistinct},
	} {
		if err := pair(c.name, c.a, c.b); err != nil {
			return err
		}
	}
	if (a.OfferedRate == 0) != (b.OfferedRate == 0) {
		return fmt.Errorf("cannot merge %s with %s: closed-loop and fixed-rate reports cannot be combined", pathA, pathB)
	}
	if a.OfferedRate > 0 {
		if err := pair("arrival", normalizedArrival(a), normalizedArrival(b)); err != nil {
			return err
		}
		if b.Digests.Response == nil {
			return fmt.Errorf("%s is fixed-rate but lacks a response-latency digest", pathB)
		}
	}
	return nil
}

// endpointMixKey canonicalizes a report's endpoint blend so two merge inputs
// must share the same shape; empty for single-endpoint reports.
func endpointMixKey(r *Report) string {
	if len(r.EndpointMix) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.EndpointMix))
	for _, s := range r.EndpointMix {
		parts = append(parts, fmt.Sprintf("%s=%g", s.Endpoint, s.Weight))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func normalizedTransport(r *Report) string {
	if r.Transport == "" {
		return "http"
	}
	return r.Transport
}

func normalizedArrival(r *Report) string {
	if r.Arrival == "" {
		return "uniform"
	}
	return r.Arrival
}

func mergedWindow(stats *loadStats, reports []*Report) string {
	if stats != nil && !stats.timeline.anchor.IsZero() {
		return stats.timeline.last.Sub(stats.timeline.anchor).Round(time.Millisecond).String()
	}
	var max time.Duration
	for _, r := range reports {
		d, err := time.ParseDuration(r.MeasuredWindow)
		if err == nil && d > max {
			max = d
		}
	}
	return max.Round(time.Millisecond).String()
}
