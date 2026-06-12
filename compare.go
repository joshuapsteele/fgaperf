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

// comparabilityCaveats returns loud warnings for differences that make a
// comparison misleading rather than merely interesting.
func comparabilityCaveats(a, b *Report) []string {
	var out []string
	if a.Endpoint != b.Endpoint {
		out = append(out, fmt.Sprintf("endpoint differs (%s vs %s); the populations are not comparable", a.Endpoint, b.Endpoint))
	}
	if a.CorpusSize != b.CorpusSize {
		out = append(out, fmt.Sprintf("corpus size differs (%d vs %d entries); per-relation populations may not line up", a.CorpusSize, b.CorpusSize))
	}
	if a.Duration != b.Duration {
		out = append(out, fmt.Sprintf("measured duration differs (%s vs %s); percentile stability differs between the runs", a.Duration, b.Duration))
	}
	if a.Concurrency != b.Concurrency {
		out = append(out, fmt.Sprintf("concurrency differs (%d vs %d workers); closed-loop throughput is not comparable", a.Concurrency, b.Concurrency))
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

func CompareMarkdown(pathA, pathB string, a, b *Report) string {
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }

	w("# fgaperf comparison")
	w("")
	w("| | A | B |")
	w("|---|---|---|")
	w("| File | %s | %s |", filepath.Base(pathA), filepath.Base(pathB))
	w("| Generated | %s | %s |", a.GeneratedAt.Format("2006-01-02 15:04 UTC"), b.GeneratedAt.Format("2006-01-02 15:04 UTC"))
	w("| Endpoint / consistency | %s / %s | %s / %s |", a.Endpoint, a.Consistency, b.Endpoint, b.Consistency)
	w("| Corpus | %d entries (%d distinct) | %d entries (%d distinct) |", a.CorpusSize, a.CorpusDistinct, b.CorpusSize, b.CorpusDistinct)
	w("")
	if caveats := comparabilityCaveats(a, b); len(caveats) > 0 {
		w("> **⚠️ These runs are not directly comparable:**")
		for _, c := range caveats {
			w("> - %s", c)
		}
		w("")
	}

	w("## Overall")
	w("")
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
	w("")

	if a.Server != nil && b.Server != nil && a.Server.DatastoreQueryCount.Count > 0 && b.Server.DatastoreQueryCount.Count > 0 {
		w("## Server-side view")
		w("")
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
		w("")
	}

	w("## Per-relation p50 / p99")
	w("")
	w("| Relation | A p50 | B p50 | Δ p50 | A p99 | B p99 | Δ p99 |")
	w("|---|---|---|---|---|---|---|")
	targets := map[string]bool{}
	for t := range a.ByTarget {
		targets[t] = true
	}
	for t := range b.ByTarget {
		targets[t] = true
	}
	names := make([]string, 0, len(targets))
	for t := range targets {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		as, aok := a.ByTarget[t]
		bs, bok := b.ByTarget[t]
		if !aok || !bok {
			w("| %s | _only in %s_ | | | | | |", t, map[bool]string{true: "A", false: "B"}[aok])
			continue
		}
		w("| %s | %s | %s | %s | %s | %s | %s |", t,
			ms(as.P50), ms(bs.P50), deltaCell(as.P50, bs.P50),
			ms(as.P99), ms(bs.P99), deltaCell(as.P99, bs.P99))
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
	a, err := LoadReport(pathA)
	if err != nil {
		return err
	}
	b, err := LoadReport(pathB)
	if err != nil {
		return err
	}
	md := CompareMarkdown(pathA, pathB, a, b)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(outDir, "compare-"+time.Now().UTC().Format("20060102-150405")+".md")
	if err := os.WriteFile(out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", out)
	return nil
}
