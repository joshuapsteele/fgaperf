package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	baselineSchemaVersion = 1
	defaultMaxRegression  = "p99=10%,throughput=-5%"
)

// Baseline is the compact, long-lived subset of a results file used by
// regression gates. It keeps enough run shape to warn about incomparable
// results without embedding the full findings JSON.
type Baseline struct {
	SchemaVersion     int                      `json:"schema_version"`
	GeneratedAt       time.Time                `json:"generated_at"`
	SourceResults     string                   `json:"source_results"`
	ToolVersion       string                   `json:"tool_version"`
	ConfigFingerprint string                   `json:"config_fingerprint"`
	RandomSeed        int64                    `json:"random_seed,omitempty"`
	Endpoint          string                   `json:"endpoint"`
	Consistency       string                   `json:"consistency"`
	Concurrency       int                      `json:"concurrency"`
	OfferedRate       int                      `json:"offered_rate"`
	Warmup            string                   `json:"warmup"`
	Duration          string                   `json:"duration"`
	CorpusSize        int                      `json:"corpus_size"`
	CorpusDistinct    int                      `json:"corpus_distinct"`
	WriteRate         int                      `json:"write_rate,omitempty"`
	Sweep             bool                     `json:"sweep,omitempty"`
	Throughput        float64                  `json:"throughput_per_sec"`
	Overall           BaselineStats            `json:"overall"`
	ByTarget          map[string]BaselineStats `json:"by_target,omitempty"`
	Server            *BaselineServer          `json:"server,omitempty"`
}

type BaselineStats struct {
	Count  int           `json:"count"`
	Errors int           `json:"errors,omitempty"`
	Mean   time.Duration `json:"mean_ns"`
	P50    time.Duration `json:"p50_ns"`
	P90    time.Duration `json:"p90_ns"`
	P95    time.Duration `json:"p95_ns"`
	P99    time.Duration `json:"p99_ns"`
	Max    time.Duration `json:"max_ns"`
}

type BaselineServer struct {
	DatastoreQueriesPerRequest float64 `json:"datastore_queries_per_request,omitempty"`
	RequestP99MS               float64 `json:"request_p99_ms,omitempty"`
}

func baselineStats(st Stats) BaselineStats {
	return BaselineStats{
		Count:  st.Count,
		Errors: st.Errors,
		Mean:   st.Mean,
		P50:    st.P50,
		P90:    st.P90,
		P95:    st.P95,
		P99:    st.P99,
		Max:    st.Max,
	}
}

func (st BaselineStats) Stats() Stats {
	return Stats{
		Count:  st.Count,
		Errors: st.Errors,
		Mean:   st.Mean,
		P50:    st.P50,
		P90:    st.P90,
		P95:    st.P95,
		P99:    st.P99,
		Max:    st.Max,
	}
}

func baselineFromReport(sourcePath string, r *Report, generatedAt time.Time) *Baseline {
	byTarget := make(map[string]BaselineStats, len(r.ByTarget))
	for target, st := range r.ByTarget {
		byTarget[target] = baselineStats(st)
	}
	seed, _ := resolvedInt64(r.ResolvedConfig, "random_seed")
	b := &Baseline{
		SchemaVersion:     baselineSchemaVersion,
		GeneratedAt:       generatedAt,
		SourceResults:     filepath.Base(sourcePath),
		ToolVersion:       r.ToolVersion,
		ConfigFingerprint: configFingerprint(r.ResolvedConfig),
		RandomSeed:        seed,
		Endpoint:          r.Endpoint,
		Consistency:       r.Consistency,
		Concurrency:       r.Concurrency,
		OfferedRate:       r.OfferedRate,
		Warmup:            r.Warmup,
		Duration:          r.Duration,
		CorpusSize:        r.CorpusSize,
		CorpusDistinct:    r.CorpusDistinct,
		WriteRate:         r.WriteRate,
		Sweep:             len(r.Sweep) > 0,
		Throughput:        r.Throughput,
		Overall:           baselineStats(r.Overall),
		ByTarget:          byTarget,
	}
	if r.Server != nil {
		b.Server = &BaselineServer{}
		if r.Server.DatastoreQueryCount.Count > 0 {
			b.Server.DatastoreQueriesPerRequest = r.Server.DatastoreQueryCount.Mean
		}
		if r.Server.RequestDuration.Count > 0 {
			b.Server.RequestP99MS = r.Server.RequestDuration.P99
		}
		if b.Server.DatastoreQueriesPerRequest == 0 && b.Server.RequestP99MS == 0 {
			b.Server = nil
		}
	}
	return b
}

func (b *Baseline) reportShape() *Report {
	byTarget := make(map[string]Stats, len(b.ByTarget))
	for target, st := range b.ByTarget {
		byTarget[target] = st.Stats()
	}
	r := &Report{
		ToolVersion:    b.ToolVersion,
		Endpoint:       b.Endpoint,
		Consistency:    b.Consistency,
		Concurrency:    b.Concurrency,
		OfferedRate:    b.OfferedRate,
		Warmup:         b.Warmup,
		Duration:       b.Duration,
		CorpusSize:     b.CorpusSize,
		CorpusDistinct: b.CorpusDistinct,
		WriteRate:      b.WriteRate,
		Throughput:     b.Throughput,
		Overall:        b.Overall.Stats(),
		ByTarget:       byTarget,
	}
	if b.Sweep {
		r.Sweep = []SweepStep{{}}
	}
	if b.Server != nil {
		r.Server = &ServerMetrics{
			RequestDuration:     HistogramSummary{Count: 1, P99: b.Server.RequestP99MS},
			DatastoreQueryCount: HistogramSummary{Count: 1, Mean: b.Server.DatastoreQueriesPerRequest},
		}
	}
	return r
}

func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if b.SchemaVersion != baselineSchemaVersion {
		return nil, fmt.Errorf("%s has unsupported baseline schema_version %d", path, b.SchemaVersion)
	}
	if b.Overall.Count == 0 && len(b.ByTarget) == 0 {
		return nil, fmt.Errorf("%s does not look like a fgaperf baseline file", path)
	}
	return &b, nil
}

func saveBaseline(resultsPath, outDir string) error {
	_, err := saveBaselineAt(resultsPath, outDir, time.Now().UTC())
	return err
}

func saveBaselineAt(resultsPath, outDir string, generatedAt time.Time) (string, error) {
	r, err := LoadReport(resultsPath)
	if err != nil {
		return "", err
	}
	b := baselineFromReport(resultsPath, r, generatedAt)
	data, err := json.MarshalIndent(b, "", " ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	artifacts, err := createArtifactSet(outDir, generatedAt.Format("20060102-150405"), []string{"baseline-%s.json"})
	if err != nil {
		return "", err
	}
	if err := writeArtifacts(artifacts, [][]byte{data}); err != nil {
		return "", err
	}
	fmt.Printf("wrote %s\n", artifacts[0].path)
	return artifacts[0].path, nil
}

func configFingerprint(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func resolvedInt64(m map[string]any, path ...string) (int64, bool) {
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
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		if v > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case float64:
		if math.Trunc(v) != v {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

type regressionThresholds map[string]float64

func parseMaxRegressions(raw string) (regressionThresholds, error) {
	out := regressionThresholds{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid max-regression %q (want metric=percent)", part)
		}
		metric, ok := canonicalRegressionMetric(k)
		if !ok {
			return nil, fmt.Errorf("unknown max-regression metric %q", strings.TrimSpace(k))
		}
		v = strings.TrimSpace(v)
		if !strings.HasSuffix(v, "%") {
			return nil, fmt.Errorf("invalid max-regression %q (only percentages are supported)", part)
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid max-regression %q: %w", part, err)
		}
		out[metric] = pct
	}
	return out, nil
}

func canonicalRegressionMetric(metric string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(metric))
	m = strings.NewReplacer("-", "_", "/", "_").Replace(m)
	switch m {
	case "mean", "p50", "p90", "p95", "p99", "max", "throughput", "server_p99":
		return m, true
	case "ds_queries", "ds_queries_request", "datastore_queries", "datastore_queries_request", "datastore_queries_per_request":
		return "ds_queries", true
	default:
		return "", false
	}
}

type baselineComparison struct {
	Baseline   *Baseline
	Current    *Report
	Thresholds regressionThresholds
	Warnings   []string
	Failures   []regressionFinding
}

type regressionFinding struct {
	Metric     string
	Target     string
	Baseline   float64
	Current    float64
	Limit      float64
	DeltaPct   float64
	AllowedPct float64
	Unit       string
	Direction  string
}

func (f regressionFinding) String() string {
	return fmt.Sprintf("%s %s regressed %+.1f%% (baseline %.2f%s, current %.2f%s, limit %s %.2f%s)",
		f.Metric, f.Target, f.DeltaPct, f.Baseline, f.Unit, f.Current, f.Unit, f.Direction, f.Limit, f.Unit)
}

func evaluateBaseline(b *Baseline, current *Report, thresholds regressionThresholds) baselineComparison {
	cmp := baselineComparison{Baseline: b, Current: current, Thresholds: thresholds}
	cmp.Warnings = append(cmp.Warnings, comparabilityCaveats(b.reportShape(), current)...)
	currentFP := configFingerprint(current.ResolvedConfig)
	if b.ConfigFingerprint != "" && currentFP != "" && b.ConfigFingerprint != currentFP {
		cmp.Warnings = append(cmp.Warnings, "resolved config fingerprint differs; regression thresholds still run, but compare the config diff before treating this as apples-to-apples")
	}
	if seed, ok := resolvedInt64(current.ResolvedConfig, "random_seed"); ok && b.RandomSeed != 0 && seed != b.RandomSeed {
		cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("random_seed differs (%d vs %d)", b.RandomSeed, seed))
	}
	for metric, allowedPct := range thresholds {
		switch metric {
		case "throughput":
			cmp.checkThroughput(allowedPct)
		case "ds_queries":
			cmp.checkServerMetric(metric, "queries/request", b.serverDSQueries(), currentDSQueries(current), allowedPct)
		case "server_p99":
			cmp.checkServerMetric(metric, "ms", b.serverRequestP99(), currentServerP99(current), allowedPct)
		default:
			cmp.checkLatencyMetric(metric, allowedPct)
		}
	}
	sort.Slice(cmp.Failures, func(i, j int) bool {
		if cmp.Failures[i].Metric == cmp.Failures[j].Metric {
			return cmp.Failures[i].Target < cmp.Failures[j].Target
		}
		return cmp.Failures[i].Metric < cmp.Failures[j].Metric
	})
	sort.Strings(cmp.Warnings)
	return cmp
}

func (cmp *baselineComparison) checkThroughput(allowedPct float64) {
	base, current := cmp.Baseline.Throughput, cmp.Current.Throughput
	if base <= 0 || current <= 0 {
		cmp.Warnings = append(cmp.Warnings, "throughput regression gate skipped because baseline or current throughput is zero")
		return
	}
	pct := allowedPct
	if pct > 0 {
		pct = -pct
	}
	limit := base * (1 + pct/100)
	if current < limit {
		cmp.Failures = append(cmp.Failures, regressionFinding{
			Metric:     "throughput",
			Target:     "overall",
			Baseline:   base,
			Current:    current,
			Limit:      limit,
			DeltaPct:   percentDelta(base, current),
			AllowedPct: pct,
			Unit:       "/s",
			Direction:  ">=",
		})
	}
}

func (cmp *baselineComparison) checkServerMetric(metric, unit string, base, current metricValue, allowedPct float64) {
	if !base.OK || !current.OK {
		cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s regression gate skipped because baseline or current server metric is absent", metric))
		return
	}
	cmp.checkIncrease(metric, "overall", base.Value, current.Value, allowedPct, unit)
}

func (cmp *baselineComparison) checkLatencyMetric(metric string, allowedPct float64) {
	base, ok := baselineStatsMetric(cmp.Baseline.Overall, metric)
	current, cok := statsMetric(cmp.Current.Overall, metric)
	if !ok || !cok {
		cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s regression gate skipped for overall because baseline or current value is absent", metric))
	} else {
		cmp.checkIncrease(metric, "overall", base, current, allowedPct, "ms")
	}
	for target, bst := range cmp.Baseline.ByTarget {
		cst, ok := cmp.Current.ByTarget[target]
		if !ok {
			cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s target %q is absent from current results", metric, target))
			continue
		}
		base, ok := baselineStatsMetric(bst, metric)
		current, cok := statsMetric(cst, metric)
		if !ok || !cok {
			cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s regression gate skipped for %s because baseline or current value is absent", metric, target))
			continue
		}
		cmp.checkIncrease(metric, target, base, current, allowedPct, "ms")
	}
}

func (cmp *baselineComparison) checkIncrease(metric, target string, base, current, allowedPct float64, unit string) {
	if base <= 0 {
		cmp.Warnings = append(cmp.Warnings, fmt.Sprintf("%s regression gate skipped for %s because baseline is zero", metric, target))
		return
	}
	pct := allowedPct
	if pct < 0 {
		pct = -pct
	}
	limit := base * (1 + pct/100)
	if current > limit {
		cmp.Failures = append(cmp.Failures, regressionFinding{
			Metric:     metric,
			Target:     target,
			Baseline:   base,
			Current:    current,
			Limit:      limit,
			DeltaPct:   percentDelta(base, current),
			AllowedPct: pct,
			Unit:       unit,
			Direction:  "<=",
		})
	}
}

func (b *Baseline) serverDSQueries() metricValue {
	if b.Server == nil || b.Server.DatastoreQueriesPerRequest == 0 {
		return metricValue{}
	}
	return metricValue{Value: b.Server.DatastoreQueriesPerRequest, OK: true}
}

func (b *Baseline) serverRequestP99() metricValue {
	if b.Server == nil || b.Server.RequestP99MS == 0 {
		return metricValue{}
	}
	return metricValue{Value: b.Server.RequestP99MS, OK: true}
}

type metricValue struct {
	Value float64
	OK    bool
}

func currentDSQueries(r *Report) metricValue {
	if r.Server == nil || r.Server.DatastoreQueryCount.Count == 0 {
		return metricValue{}
	}
	return metricValue{Value: r.Server.DatastoreQueryCount.Mean, OK: true}
}

func currentServerP99(r *Report) metricValue {
	if r.Server == nil || r.Server.RequestDuration.Count == 0 {
		return metricValue{}
	}
	return metricValue{Value: r.Server.RequestDuration.P99, OK: true}
}

func baselineStatsMetric(st BaselineStats, metric string) (float64, bool) {
	return statsMetric(st.Stats(), metric)
}

func statsMetric(st Stats, metric string) (float64, bool) {
	if st.Count == 0 {
		return 0, false
	}
	switch metric {
	case "mean":
		return durationMS(st.Mean), true
	case "p50":
		return durationMS(st.P50), true
	case "p90":
		return durationMS(st.P90), true
	case "p95":
		return durationMS(st.P95), true
	case "p99":
		return durationMS(st.P99), true
	case "max":
		return durationMS(st.Max), true
	default:
		return 0, false
	}
}

func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func percentDelta(base, current float64) float64 {
	if base == 0 {
		return 0
	}
	return 100 * (current - base) / base
}

func compareAgainstBaseline(baselinePath, resultsPath, outDir, maxRegression string, exitOnRegression bool) error {
	return compareAgainstBaselineAt(baselinePath, resultsPath, outDir, maxRegression, exitOnRegression, time.Now().UTC())
}

func compareAgainstBaselineAt(baselinePath, resultsPath, outDir, maxRegression string, exitOnRegression bool, generatedAt time.Time) error {
	b, err := LoadBaseline(baselinePath)
	if err != nil {
		return err
	}
	r, err := LoadReport(resultsPath)
	if err != nil {
		return err
	}
	thresholds, err := parseMaxRegressions(maxRegression)
	if err != nil {
		return err
	}
	cmp := evaluateBaseline(b, r, thresholds)
	for _, w := range cmp.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	md := BaselineCompareMarkdown(baselinePath, resultsPath, cmp)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	artifacts, err := createArtifactSet(outDir, generatedAt.Format("20060102-150405"), []string{"baseline-compare-%s.md"})
	if err != nil {
		return err
	}
	if err := writeArtifacts(artifacts, [][]byte{[]byte(md)}); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", artifacts[0].path)
	if len(cmp.Failures) > 0 {
		if exitOnRegression {
			return fmt.Errorf("baseline regression: %s", strings.Join(cmp.failureStrings(), "; "))
		}
		// Advisory mode: surface the breaches but let the command succeed so a
		// trend dashboard or non-blocking PR check can record them without
		// failing the job.
		for _, f := range cmp.Failures {
			fmt.Fprintf(os.Stderr, "regression (advisory, gate disabled): %s\n", f.String())
		}
		fmt.Println("baseline comparison reported regressions (advisory mode; -exit-on-regression=false)")
		return nil
	}
	fmt.Println("baseline comparison passed")
	return nil
}

func (cmp baselineComparison) failureStrings() []string {
	out := make([]string, 0, len(cmp.Failures))
	for _, f := range cmp.Failures {
		out = append(out, f.String())
	}
	return out
}

func BaselineCompareMarkdown(baselinePath, resultsPath string, cmp baselineComparison) string {
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }
	w("# fgaperf baseline comparison")
	w("")
	w("| | Baseline | Current |")
	w("|---|---|---|")
	w("| File | %s | %s |", filepath.Base(baselinePath), filepath.Base(resultsPath))
	w("| Source results | %s | %s |", cmp.Baseline.SourceResults, filepath.Base(resultsPath))
	w("| Endpoint / consistency | %s / %s | %s / %s |", cmp.Baseline.Endpoint, cmp.Baseline.Consistency, cmp.Current.Endpoint, cmp.Current.Consistency)
	w("| Corpus | %d entries (%d distinct) | %d entries (%d distinct) |", cmp.Baseline.CorpusSize, cmp.Baseline.CorpusDistinct, cmp.Current.CorpusSize, cmp.Current.CorpusDistinct)
	w("")
	if len(cmp.Warnings) > 0 {
		w("## Warnings")
		w("")
		for _, warning := range cmp.Warnings {
			w("- %s", warning)
		}
		w("")
	}
	w("## Regression Gates")
	w("")
	w("| Metric | Max regression |")
	w("|---|---|")
	for _, metric := range sortedThresholdMetrics(cmp.Thresholds) {
		w("| %s | %+.1f%% |", metric, cmp.Thresholds[metric])
	}
	if len(cmp.Thresholds) == 0 {
		w("| _none_ | |")
	}
	w("")
	if len(cmp.Failures) > 0 {
		w("## Failures")
		w("")
		for _, f := range cmp.Failures {
			w("- %s", f.String())
		}
		w("")
	} else {
		w("## Result")
		w("")
		w("All configured regression gates passed.")
		w("")
	}
	w("## Overall")
	w("")
	w("| Metric | Baseline | Current | Δ |")
	w("|---|---|---|---|")
	w("| Throughput | %.0f/s | %.0f/s | %+.1f%% |", cmp.Baseline.Throughput, cmp.Current.Throughput, percentDelta(cmp.Baseline.Throughput, cmp.Current.Throughput))
	overall := cmp.Baseline.Overall.Stats()
	row := func(metric string, base, current time.Duration) {
		w("| %s | %s ms | %s ms | %+.1f%% |", metric, ms(base), ms(current), percentDelta(durationMS(base), durationMS(current)))
	}
	row("p50", overall.P50, cmp.Current.Overall.P50)
	row("p90", overall.P90, cmp.Current.Overall.P90)
	row("p95", overall.P95, cmp.Current.Overall.P95)
	row("p99", overall.P99, cmp.Current.Overall.P99)
	w("")
	if cmp.Baseline.Server != nil || cmp.Current.Server != nil {
		w("## Server-side")
		w("")
		w("| Metric | Baseline | Current |")
		w("|---|---|---|")
		w("| Datastore queries/request | %s | %s |", formatMetricValue(cmp.Baseline.serverDSQueries()), formatMetricValue(currentDSQueries(cmp.Current)))
		w("| Server p99 | %s ms | %s ms |", formatMetricValue(cmp.Baseline.serverRequestP99()), formatMetricValue(currentServerP99(cmp.Current)))
		w("")
	}
	if len(cmp.Baseline.ByTarget) > 0 {
		w("## Per-target p99")
		w("")
		w("| Target | Baseline | Current | Δ |")
		w("|---|---|---|---|")
		targets := make([]string, 0, len(cmp.Baseline.ByTarget))
		for target := range cmp.Baseline.ByTarget {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			bst := cmp.Baseline.ByTarget[target].Stats()
			cst, ok := cmp.Current.ByTarget[target]
			if !ok {
				w("| %s | %s ms | _missing_ | |", target, ms(bst.P99))
				continue
			}
			w("| %s | %s ms | %s ms | %+.1f%% |", target, ms(bst.P99), ms(cst.P99), percentDelta(durationMS(bst.P99), durationMS(cst.P99)))
		}
		w("")
	}
	return sb.String()
}

func sortedThresholdMetrics(th regressionThresholds) []string {
	metrics := make([]string, 0, len(th))
	for metric := range th {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics)
	return metrics
}

func formatMetricValue(v metricValue) string {
	if !v.OK {
		return "—"
	}
	return fmt.Sprintf("%.2f", v.Value)
}
