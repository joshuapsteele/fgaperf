package main

// report_html.go renders a self-contained visual report from the same Report
// struct used by the JSON and Markdown outputs. It deliberately avoids
// JavaScript, external stylesheets, fonts, and images so CI artifacts can be
// opened offline.

import (
	"fmt"
	"html"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	htmlChartW = 760
	htmlChartH = 300
)

type htmlPoint struct {
	X float64
	Y float64
}

func (r *Report) HTML() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	transport := "HTTP"
	if r.Transport == "grpc" {
		transport = "gRPC"
	}
	rateMode := "closed loop"
	if r.OfferedRate > 0 {
		rateMode = fmt.Sprintf("%d req/s", r.OfferedRate)
		if r.Arrival == "poisson" {
			rateMode += " poisson"
		}
	}

	w("<!doctype html>")
	w("<html lang=\"en\">")
	w("<head>")
	w("<meta charset=\"utf-8\">")
	w("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	w("<title>fgaperf report - %s</title>", htmlText(r.GeneratedAt.Format("2006-01-02 15:04 UTC")))
	w("<style>")
	w("%s", htmlReportCSS())
	w("</style>")
	w("</head>")
	w("<body>")
	w("<main class=\"page\">")
	w("<header class=\"hero\">")
	w("<p class=\"eyebrow\">fgaperf %s</p>", htmlText(r.ToolVersion))
	w("<h1>OpenFGA performance report</h1>")
	w("<p class=\"summary\">%s</p>", htmlText(r.summary()))
	w("<dl class=\"run-meta\">")
	htmlMeta(&b, "Generated", r.GeneratedAt.Format("2006-01-02 15:04 UTC"))
	htmlMeta(&b, "Endpoint", r.Endpoint)
	htmlMeta(&b, "Transport", transport)
	htmlMeta(&b, "Mode", rateMode)
	htmlMeta(&b, "Concurrency", fmt.Sprintf("%d workers", r.Concurrency))
	htmlMeta(&b, "Measured", fmt.Sprintf("%s window", r.MeasuredWindow))
	w("</dl>")
	w("</header>")

	w("<section class=\"kpis\" aria-label=\"Headline metrics\">")
	htmlKPI(&b, "Throughput", fmt.Sprintf("%.0f", r.Throughput), endpointNoun(r.Endpoint)+"/sec")
	htmlKPI(&b, "Client p99", ms(r.Overall.P99), "ms")
	if r.ResponseLatency != nil {
		htmlKPI(&b, "Response p99", ms(r.ResponseLatency.P99), "ms")
	}
	if r.SweepKneeRate > 0 {
		htmlKPI(&b, "Sweep knee", fmt.Sprintf("%d", r.SweepKneeRate), "req/s")
	} else if r.OfferedRate > 0 {
		htmlKPI(&b, "Achieved", fmt.Sprintf("%.0f", r.AchievedRate), "req/s")
	}
	htmlKPI(&b, "Errors", fmt.Sprintf("%d", r.Overall.Errors), "requests")
	htmlKPI(&b, "Mismatches", fmt.Sprintf("%d", r.Mismatches), "responses")
	w("</section>")

	if r.MismatchFile != "" {
		w("<p class=\"notice\">Mismatched checks were written to <code>%s</code>.</p>", htmlText(filepath.Base(r.MismatchFile)))
	}
	if len(r.MergedFrom) > 0 {
		w("<p class=\"notice\">This report merges %d client-side result files. Throughput is summed and latency percentiles are merged from digest sketches.</p>", len(r.MergedFrom))
	}
	if r.Interim {
		w("<p class=\"notice\">Interim soak snapshot #%d. The final run report remains the authoritative whole-window result.</p>", r.InterimIndex)
	}

	w("<section class=\"chart-grid\" aria-label=\"Charts\">")
	if len(r.Sweep) > 0 {
		htmlChartCard(&b, "Rate sweep keep-up curve", "Achieved request rate compared with the offered rate. The knee marker is the highest sustained step that met the SLO, when one was configured.", r.sweepSVG())
	}
	if len(r.Timeline) >= 2 {
		htmlChartCard(&b, "Latency over time", "Measured-window buckets. p99 spikes expose pauses, cache fill-in, or saturation that aggregate percentiles hide.", r.timelineSVG())
	}
	if r.Overall.Count > 0 {
		htmlChartCard(&b, "Latency distribution", "Approximate cumulative curve from the retained percentile summary. Fixed-rate runs also show response latency when present.", r.percentileSVG())
	}
	if len(r.ByTarget) > 0 {
		title := "Per-relation latency"
		if r.Endpoint == "batch-check" {
			title = "Batch latency"
		}
		htmlChartCard(&b, title, "p99 bars by target, sorted slowest first. The dark tick marks each target's p50.", r.targetBarsSVG())
	}
	w("</section>")

	w("<section class=\"section\">")
	w("<h2>Headline results</h2>")
	w("<div class=\"table-wrap\">")
	w("<table>")
	w("<thead><tr><th>Population</th><th>Requests</th><th>Errors</th><th>Mean</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th><th>Max</th></tr></thead>")
	w("<tbody>")
	htmlStatsRow(&b, "All checks", r.Overall)
	if r.ResponseLatency != nil {
		htmlStatsRow(&b, "All checks (response latency)", *r.ResponseLatency)
	}
	htmlStatsRow(&b, "CEL-conditioned paths", r.Conditioned)
	htmlStatsRow(&b, "Unconditioned paths", r.Unconditioned)
	htmlStatsRow(&b, "With contextual tuples", r.Contextual)
	htmlStatsRow(&b, "Without contextual tuples", r.NoContextual)
	if r.WriteChurn != nil {
		htmlStatsRow(&b, "Background tuple writes", *r.WriteChurn)
	}
	w("</tbody>")
	w("</table>")
	w("</div>")
	w("</section>")

	if len(r.Sweep) > 0 {
		w("<section class=\"section\">")
		w("<h2>Rate sweep</h2>")
		w("<div class=\"table-wrap\">")
		w("<table>")
		w("<thead><tr><th>Offered req/s</th><th>Achieved</th><th>Dropped slots</th><th>p50</th><th>p95</th><th>p99</th><th>Response p99</th><th>Errors</th></tr></thead>")
		w("<tbody>")
		for _, st := range r.Sweep {
			offered := fmt.Sprintf("%d", st.OfferedRate)
			if st.OfferedRate == r.SweepKneeRate && r.SweepKneeRate > 0 {
				offered += " knee"
			}
			w("<tr><td>%s</td><td>%.0f</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td></tr>",
				htmlText(offered), st.AchievedRate, st.DroppedSlots, ms(st.Overall.P50), ms(st.Overall.P95), ms(st.Overall.P99), ms(st.ResponseLatency.P99), st.Overall.Errors)
		}
		w("</tbody>")
		w("</table>")
		w("</div>")
		w("</section>")
	}

	if r.ResultCounts != nil {
		c := r.ResultCounts
		w("<section class=\"section split\">")
		w("<div>")
		w("<h2>Result-set sizes</h2>")
		w("<p>Returned-set size is the main cost driver for %s calls.</p>", htmlText(r.Endpoint))
		w("</div>")
		w("<dl class=\"mini-stats\">")
		htmlMeta(&b, "Responses", fmt.Sprintf("%d", c.Responses))
		htmlMeta(&b, "Mean", fmt.Sprintf("%.1f", c.Mean))
		htmlMeta(&b, "p99", fmt.Sprintf("%d", c.P99))
		htmlMeta(&b, "Max", fmt.Sprintf("%d", c.Max))
		htmlMeta(&b, "Empty", fmt.Sprintf("%d", c.Empty))
		w("</dl>")
		w("</section>")
	}

	if len(r.ByTarget) > 0 {
		w("<section class=\"section\">")
		if r.Endpoint == "batch-check" {
			w("<h2>Batch breakdown</h2>")
		} else {
			w("<h2>Per-relation breakdown</h2>")
		}
		w("<div class=\"table-wrap\">")
		w("<table>")
		dsCol := r.Endpoint != "batch-check" && len(r.DSQueriesByTarget) > 0
		if dsCol {
			w("<thead><tr><th>Target</th><th>Requests</th><th>Errors</th><th>Mean</th><th>p50</th><th>p95</th><th>p99</th><th>DS queries/check</th></tr></thead>")
		} else {
			w("<thead><tr><th>Target</th><th>Requests</th><th>Errors</th><th>Mean</th><th>p50</th><th>p95</th><th>p99</th></tr></thead>")
		}
		w("<tbody>")
		for _, target := range r.sortedTargetNames() {
			s := r.ByTarget[target]
			if s.Count == 0 && s.Errors == 0 {
				continue
			}
			if dsCol {
				dsq := ""
				if v, ok := r.DSQueriesByTarget[target]; ok {
					dsq = fmt.Sprintf("%.1f", v)
				}
				w("<tr><td><code>%s</code></td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
					htmlText(target), s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99), htmlText(dsq))
			} else {
				w("<tr><td><code>%s</code></td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
					htmlText(target), s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P95), ms(s.P99))
			}
		}
		w("</tbody>")
		w("</table>")
		w("</div>")
		w("</section>")
	}

	if len(r.ErrorsByClass) > 0 {
		w("<section class=\"section split\">")
		w("<div>")
		w("<h2>Errors</h2>")
		w("<p>Failures grouped by class. Timeout and 5xx usually point at server or datastore pressure; 4xx and decode errors usually point at workload shape or config.</p>")
		w("</div>")
		w("<dl class=\"mini-stats\">")
		for _, class := range sortedKeys(r.ErrorsByClass) {
			htmlMeta(&b, class, fmt.Sprintf("%d", r.ErrorsByClass[class]))
		}
		w("</dl>")
		if len(r.ErrorSamples) > 0 {
			w("<div class=\"error-samples\">")
			w("<h3>First messages</h3>")
			w("<ul>")
			for _, sample := range r.ErrorSamples {
				w("<li><code>%s</code></li>", htmlText(sample))
			}
			w("</ul>")
			w("</div>")
		}
		w("</section>")
	}

	if r.Server != nil && r.Server.RequestDuration.Count > 0 {
		s := r.Server
		w("<section class=\"section split\">")
		w("<div>")
		w("<h2>Server-side view</h2>")
		w("<p>OpenFGA metrics diffed over the measured phase. These values exclude client-side transport overhead.</p>")
		w("</div>")
		w("<dl class=\"mini-stats\">")
		htmlMeta(&b, "Server p99", fmt.Sprintf("%.2f ms", s.RequestDuration.P99))
		htmlMeta(&b, "Server mean", fmt.Sprintf("%.2f ms", s.RequestDuration.Mean))
		if s.DatastoreQueryCount.Count > 0 {
			htmlMeta(&b, "DS queries/request", fmt.Sprintf("%.2f", s.DatastoreQueryCount.Mean))
			htmlMeta(&b, "Total DS queries", fmt.Sprintf("%.0f", s.TotalDatastoreQueries))
		}
		if s.CheckCacheTotal > 0 {
			htmlMeta(&b, "Check cache hit rate", fmt.Sprintf("%.1f%%", 100*s.CheckCacheHits/s.CheckCacheTotal))
		}
		w("</dl>")
		w("</section>")
	}

	w("<footer>")
	w("Self-contained report generated by fgaperf. JSON and Markdown artifacts carry the same measurements for automation and PR review.")
	w("</footer>")
	w("</main>")
	w("</body>")
	w("</html>")
	return b.String()
}

func htmlReportCSS() string {
	return `
:root {
  color-scheme: light;
  --ink: #17202a;
  --muted: #5d6876;
  --line: #d9dee7;
  --paper: #f7f8fb;
  --card: #ffffff;
  --teal: #0f766e;
  --blue: #2563eb;
  --coral: #e11d48;
  --amber: #b45309;
  --violet: #6d28d9;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--paper);
  color: var(--ink);
  font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
.page {
  max-width: 1180px;
  margin: 0 auto;
  padding: 28px 20px 40px;
}
.hero, .section, .chart-card, .kpi {
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 8px;
}
.hero {
  padding: 28px;
}
.eyebrow {
  margin: 0 0 8px;
  color: var(--teal);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: .08em;
  text-transform: uppercase;
}
h1, h2, h3, p { margin-top: 0; }
h1 {
  margin-bottom: 10px;
  font-size: 42px;
  line-height: 1.05;
}
h2 {
  margin-bottom: 10px;
  font-size: 20px;
}
h3 {
  margin-bottom: 8px;
  font-size: 15px;
}
.summary {
  max-width: 900px;
  color: var(--muted);
  font-size: 16px;
}
.run-meta, .mini-stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 12px 18px;
  margin: 20px 0 0;
}
.run-meta div, .mini-stats div {
  min-width: 0;
}
dt {
  color: var(--muted);
  font-size: 12px;
  font-weight: 700;
  text-transform: uppercase;
}
dd {
  margin: 3px 0 0;
  overflow-wrap: anywhere;
  font-weight: 700;
}
.kpis {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 12px;
  margin: 16px 0;
}
.kpi {
  padding: 16px;
}
.kpi strong {
  display: block;
  font-size: 28px;
  line-height: 1;
}
.kpi span {
  color: var(--muted);
  font-size: 12px;
  font-weight: 700;
  text-transform: uppercase;
}
.kpi small {
  color: var(--muted);
}
.notice {
  margin: 12px 0;
  padding: 12px 14px;
  border: 1px solid #f2d28c;
  border-radius: 8px;
  background: #fff8e8;
}
.chart-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 480px), 1fr));
  gap: 16px;
  margin: 16px 0;
}
.chart-card {
  padding: 18px;
}
.chart-card p {
  color: var(--muted);
}
.chart {
  width: 100%;
  height: auto;
  display: block;
}
.axis, .grid {
  stroke: #c5ccd8;
  stroke-width: 1;
}
.grid {
  stroke-dasharray: 3 5;
}
.label, .tick {
  fill: var(--muted);
  font-size: 12px;
}
.series-p99, .series-achieved {
  fill: none;
  stroke: var(--blue);
  stroke-width: 3;
}
.series-p50, .series-response {
  fill: none;
  stroke: var(--teal);
  stroke-width: 2;
}
.series-ideal {
  fill: none;
  stroke: var(--muted);
  stroke-width: 2;
  stroke-dasharray: 5 6;
}
.bar-p99 {
  fill: var(--blue);
}
.bar-p50 {
  fill: var(--teal);
}
.point-ok {
  fill: var(--teal);
  stroke: #fff;
  stroke-width: 2;
}
.point-hot {
  fill: var(--coral);
  stroke: #fff;
  stroke-width: 2;
}
.point-knee {
  fill: var(--amber);
  stroke: #fff;
  stroke-width: 2;
}
.section {
  margin: 16px 0;
  padding: 18px;
}
.split {
  display: grid;
  grid-template-columns: minmax(220px, .7fr) minmax(260px, 1fr);
  gap: 18px;
}
.table-wrap {
  overflow-x: auto;
}
table {
  width: 100%;
  border-collapse: collapse;
}
th, td {
  padding: 9px 10px;
  border-bottom: 1px solid var(--line);
  text-align: right;
  white-space: nowrap;
}
th:first-child, td:first-child {
  text-align: left;
}
th {
  color: var(--muted);
  font-size: 12px;
  text-transform: uppercase;
}
code {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: .95em;
}
.error-samples {
  grid-column: 1 / -1;
}
footer {
  margin-top: 24px;
  color: var(--muted);
  font-size: 12px;
}
@media (max-width: 720px) {
  .page { padding: 16px 12px 28px; }
  .hero, .section, .chart-card { padding: 16px; }
  h1 { font-size: 30px; }
  .split { grid-template-columns: 1fr; }
  th, td { padding: 8px; }
}
`
}

func htmlMeta(b *strings.Builder, k, v string) {
	fmt.Fprintf(b, "<div><dt>%s</dt><dd>%s</dd></div>\n", htmlText(k), htmlText(v))
}

func htmlKPI(b *strings.Builder, label, value, suffix string) {
	fmt.Fprintf(b, "<div class=\"kpi\"><span>%s</span><strong>%s</strong><small>%s</small></div>\n",
		htmlText(label), htmlText(value), htmlText(suffix))
}

func htmlChartCard(b *strings.Builder, title, desc, svg string) {
	fmt.Fprintf(b, "<article class=\"chart-card\"><h2>%s</h2><p>%s</p>%s</article>\n",
		htmlText(title), htmlText(desc), svg)
}

func htmlStatsRow(b *strings.Builder, label string, s Stats) {
	if s.Count == 0 && s.Errors == 0 {
		return
	}
	fmt.Fprintf(b, "<tr><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
		htmlText(label), s.Count, s.Errors, ms(s.Mean), ms(s.P50), ms(s.P90), ms(s.P95), ms(s.P99), ms(s.Max))
}

func (r *Report) sweepSVG() string {
	if len(r.Sweep) == 0 {
		return ""
	}
	var maxRate float64
	points := make([]htmlPoint, 0, len(r.Sweep))
	for _, st := range r.Sweep {
		maxRate = math.Max(maxRate, float64(st.OfferedRate))
		maxRate = math.Max(maxRate, st.AchievedRate)
	}
	if maxRate <= 0 {
		maxRate = 1
	}
	plot := htmlPlot{Left: 64, Top: 24, Right: 24, Bottom: 48, W: htmlChartW, H: htmlChartH}
	for _, st := range r.Sweep {
		points = append(points, htmlPoint{
			X: plot.x(float64(st.OfferedRate), 0, maxRate),
			Y: plot.y(st.AchievedRate, 0, maxRate),
		})
	}
	var b strings.Builder
	htmlSVGStart(&b, "Rate sweep keep-up curve")
	htmlGrid(&b, plot, maxRate, "req/s")
	fmt.Fprintf(&b, "<path class=\"series-ideal\" d=\"M %.1f %.1f L %.1f %.1f\" />\n",
		plot.x(0, 0, maxRate), plot.y(0, 0, maxRate), plot.x(maxRate, 0, maxRate), plot.y(maxRate, 0, maxRate))
	fmt.Fprintf(&b, "<polyline class=\"series-achieved\" points=\"%s\" />\n", htmlPolyline(points))
	for _, st := range r.Sweep {
		class := "point-ok"
		if st.Saturated || !st.PassesSLO {
			class = "point-hot"
		}
		if st.OfferedRate == r.SweepKneeRate && r.SweepKneeRate > 0 {
			class = "point-knee"
		}
		x := plot.x(float64(st.OfferedRate), 0, maxRate)
		y := plot.y(st.AchievedRate, 0, maxRate)
		fmt.Fprintf(&b, "<circle class=\"%s\" cx=\"%.1f\" cy=\"%.1f\" r=\"5\"><title>%d offered, %.0f achieved</title></circle>\n",
			class, x, y, st.OfferedRate, st.AchievedRate)
		if st.OfferedRate == r.SweepKneeRate && r.SweepKneeRate > 0 {
			fmt.Fprintf(&b, "<text class=\"label\" x=\"%.1f\" y=\"%.1f\">knee</text>\n", x+8, math.Max(18, y-8))
		}
	}
	htmlAxisLabels(&b, plot, "Offered req/s", "Achieved req/s")
	htmlSVGLegend(&b, []htmlLegendItem{{"Achieved", "series-achieved"}, {"Ideal", "series-ideal"}})
	b.WriteString("</svg>")
	return b.String()
}

func (r *Report) timelineSVG() string {
	if len(r.Timeline) < 2 {
		return ""
	}
	var maxLatency float64
	var maxSec int
	p50 := make([]htmlPoint, 0, len(r.Timeline))
	p99 := make([]htmlPoint, 0, len(r.Timeline))
	for _, tb := range r.Timeline {
		maxLatency = math.Max(maxLatency, htmlDurationMillis(tb.P99))
		maxLatency = math.Max(maxLatency, htmlDurationMillis(tb.P50))
		if tb.OffsetSec > maxSec {
			maxSec = tb.OffsetSec
		}
	}
	if maxLatency <= 0 {
		maxLatency = 1
	}
	if maxSec <= 0 {
		maxSec = len(r.Timeline) - 1
	}
	plot := htmlPlot{Left: 64, Top: 24, Right: 24, Bottom: 48, W: htmlChartW, H: htmlChartH}
	for i, tb := range r.Timeline {
		xVal := float64(tb.OffsetSec)
		if xVal == 0 && maxSec == len(r.Timeline)-1 {
			xVal = float64(i)
		}
		p50 = append(p50, htmlPoint{X: plot.x(xVal, 0, float64(maxSec)), Y: plot.y(htmlDurationMillis(tb.P50), 0, maxLatency)})
		p99 = append(p99, htmlPoint{X: plot.x(xVal, 0, float64(maxSec)), Y: plot.y(htmlDurationMillis(tb.P99), 0, maxLatency)})
	}
	var b strings.Builder
	htmlSVGStart(&b, "Latency over time")
	htmlGrid(&b, plot, maxLatency, "ms")
	fmt.Fprintf(&b, "<polyline class=\"series-p99\" points=\"%s\" />\n", htmlPolyline(p99))
	fmt.Fprintf(&b, "<polyline class=\"series-p50\" points=\"%s\" />\n", htmlPolyline(p50))
	for _, tb := range r.Timeline {
		if tb.Errors == 0 {
			continue
		}
		x := plot.x(float64(tb.OffsetSec), 0, float64(maxSec))
		y := plot.y(htmlDurationMillis(tb.P99), 0, maxLatency)
		fmt.Fprintf(&b, "<circle class=\"point-hot\" cx=\"%.1f\" cy=\"%.1f\" r=\"4\"><title>%s: %d errors</title></circle>\n",
			x, y, htmlText(tb.Offset), tb.Errors)
	}
	htmlAxisLabels(&b, plot, "Time offset", "Latency ms")
	htmlSVGLegend(&b, []htmlLegendItem{{"p99", "series-p99"}, {"p50", "series-p50"}})
	b.WriteString("</svg>")
	return b.String()
}

func (r *Report) percentileSVG() string {
	points := r.percentilePoints(r.Overall)
	if len(points) == 0 {
		return ""
	}
	response := []htmlPoint(nil)
	var maxLatency float64
	for _, p := range points {
		maxLatency = math.Max(maxLatency, p.Y)
	}
	if r.ResponseLatency != nil {
		response = r.percentilePoints(*r.ResponseLatency)
		for _, p := range response {
			maxLatency = math.Max(maxLatency, p.Y)
		}
	}
	if maxLatency <= 0 {
		maxLatency = 1
	}
	plot := htmlPlot{Left: 64, Top: 24, Right: 24, Bottom: 48, W: htmlChartW, H: htmlChartH}
	svc := make([]htmlPoint, 0, len(points))
	for _, p := range points {
		svc = append(svc, htmlPoint{X: plot.x(p.X, 0, 100), Y: plot.y(p.Y, 0, maxLatency)})
	}
	resp := make([]htmlPoint, 0, len(response))
	for _, p := range response {
		resp = append(resp, htmlPoint{X: plot.x(p.X, 0, 100), Y: plot.y(p.Y, 0, maxLatency)})
	}
	var b strings.Builder
	htmlSVGStart(&b, "Latency distribution")
	htmlGrid(&b, plot, maxLatency, "ms")
	fmt.Fprintf(&b, "<polyline class=\"series-p99\" points=\"%s\" />\n", htmlPolyline(svc))
	if len(resp) > 0 {
		fmt.Fprintf(&b, "<polyline class=\"series-response\" points=\"%s\" />\n", htmlPolyline(resp))
	}
	htmlAxisLabels(&b, plot, "Percentile", "Latency ms")
	legend := []htmlLegendItem{{"Service", "series-p99"}}
	if len(resp) > 0 {
		legend = append(legend, htmlLegendItem{"Response", "series-response"})
	}
	htmlSVGLegend(&b, legend)
	b.WriteString("</svg>")
	return b.String()
}

func (r *Report) percentilePoints(s Stats) []htmlPoint {
	if s.Count == 0 {
		return nil
	}
	return []htmlPoint{
		{X: 0, Y: htmlDurationMillis(s.Min)},
		{X: 50, Y: htmlDurationMillis(s.P50)},
		{X: 90, Y: htmlDurationMillis(s.P90)},
		{X: 95, Y: htmlDurationMillis(s.P95)},
		{X: 99, Y: htmlDurationMillis(s.P99)},
		{X: 100, Y: htmlDurationMillis(s.Max)},
	}
}

func (r *Report) targetBarsSVG() string {
	targets := r.sortedTargetsByP99()
	if len(targets) == 0 {
		return ""
	}
	if len(targets) > 12 {
		targets = targets[:12]
	}
	var maxP99 float64
	for _, target := range targets {
		maxP99 = math.Max(maxP99, htmlDurationMillis(r.ByTarget[target].P99))
	}
	if maxP99 <= 0 {
		maxP99 = 1
	}
	rowH := 34
	h := 86 + rowH*len(targets)
	plot := htmlPlot{Left: 230, Top: 28, Right: 78, Bottom: 34, W: htmlChartW, H: h}
	var b strings.Builder
	htmlSVGStartSize(&b, "Per-relation latency", htmlChartW, h)
	fmt.Fprintf(&b, "<line class=\"axis\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" />\n", float64(plot.Left), float64(h-plot.Bottom), float64(htmlChartW-plot.Right), float64(h-plot.Bottom))
	for i, target := range targets {
		s := r.ByTarget[target]
		y := float64(plot.Top + i*rowH)
		barW := plot.width() * htmlDurationMillis(s.P99) / maxP99
		p50W := plot.width() * htmlDurationMillis(s.P50) / maxP99
		label := shortenLabel(target, 34)
		fmt.Fprintf(&b, "<text class=\"tick\" x=\"16\" y=\"%.1f\"><title>%s</title>%s</text>\n", y+17, htmlText(target), htmlText(label))
		fmt.Fprintf(&b, "<rect class=\"bar-p99\" x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"18\" rx=\"3\"><title>%s p99 %s ms</title></rect>\n",
			float64(plot.Left), y+2, math.Max(1, barW), htmlText(target), ms(s.P99))
		fmt.Fprintf(&b, "<rect class=\"bar-p50\" x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"4\" rx=\"2\"><title>%s p50 %s ms</title></rect>\n",
			float64(plot.Left), y+9, math.Max(1, p50W), htmlText(target), ms(s.P50))
		fmt.Fprintf(&b, "<text class=\"label\" x=\"%.1f\" y=\"%.1f\">%s ms</text>\n", float64(plot.Left)+barW+8, y+17, ms(s.P99))
	}
	fmt.Fprintf(&b, "<text class=\"label\" x=\"%.1f\" y=\"%.1f\">p99 scale max %s ms</text>\n", float64(plot.Left), float64(h-10), ms(time.Duration(maxP99*float64(time.Millisecond))))
	htmlSVGLegendAt(&b, []htmlLegendItem{{"p99", "bar-p99"}, {"p50", "bar-p50"}}, htmlChartW-180, 18)
	b.WriteString("</svg>")
	return b.String()
}

func (r *Report) sortedTargetNames() []string {
	targets := make([]string, 0, len(r.ByTarget))
	for target := range r.ByTarget {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

func (r *Report) sortedTargetsByP99() []string {
	targets := r.sortedTargetNames()
	sort.SliceStable(targets, func(i, j int) bool {
		a, b := r.ByTarget[targets[i]], r.ByTarget[targets[j]]
		if a.P99 == b.P99 {
			return targets[i] < targets[j]
		}
		return a.P99 > b.P99
	})
	return targets
}

type htmlPlot struct {
	Left   int
	Top    int
	Right  int
	Bottom int
	W      int
	H      int
}

func (p htmlPlot) width() float64 {
	return float64(p.W - p.Left - p.Right)
}

func (p htmlPlot) height() float64 {
	return float64(p.H - p.Top - p.Bottom)
}

func (p htmlPlot) x(v, min, max float64) float64 {
	if max <= min {
		return float64(p.Left) + p.width()/2
	}
	return float64(p.Left) + (v-min)/(max-min)*p.width()
}

func (p htmlPlot) y(v, min, max float64) float64 {
	if max <= min {
		return float64(p.Top) + p.height()/2
	}
	return float64(p.H-p.Bottom) - (v-min)/(max-min)*p.height()
}

func htmlSVGStart(b *strings.Builder, label string) {
	htmlSVGStartSize(b, label, htmlChartW, htmlChartH)
}

func htmlSVGStartSize(b *strings.Builder, label string, w, h int) {
	fmt.Fprintf(b, "<svg class=\"chart\" role=\"img\" aria-label=\"%s\" viewBox=\"0 0 %d %d\" xmlns=\"http://www.w3.org/2000/svg\">\n", htmlText(label), w, h)
	fmt.Fprintf(b, "<title>%s</title>\n", htmlText(label))
}

func htmlGrid(b *strings.Builder, p htmlPlot, maxY float64, unit string) {
	left, right := float64(p.Left), float64(p.W-p.Right)
	top, bottom := float64(p.Top), float64(p.H-p.Bottom)
	fmt.Fprintf(b, "<line class=\"axis\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" />\n", left, bottom, right, bottom)
	fmt.Fprintf(b, "<line class=\"axis\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" />\n", left, top, left, bottom)
	for i := 0; i <= 4; i++ {
		y := bottom - float64(i)/4*p.height()
		val := maxY * float64(i) / 4
		fmt.Fprintf(b, "<line class=\"grid\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" />\n", left, y, right, y)
		fmt.Fprintf(b, "<text class=\"tick\" x=\"8\" y=\"%.1f\">%.0f %s</text>\n", y+4, val, htmlText(unit))
	}
}

func htmlAxisLabels(b *strings.Builder, p htmlPlot, xLabel, yLabel string) {
	fmt.Fprintf(b, "<text class=\"label\" x=\"%.1f\" y=\"%.1f\">%s</text>\n", float64(p.Left)+p.width()/2-38, float64(p.H-10), htmlText(xLabel))
	fmt.Fprintf(b, "<text class=\"label\" x=\"%.1f\" y=\"18\">%s</text>\n", float64(p.Left), htmlText(yLabel))
}

type htmlLegendItem struct {
	Label string
	Class string
}

func htmlSVGLegend(b *strings.Builder, items []htmlLegendItem) {
	htmlSVGLegendAt(b, items, htmlChartW-170, 18)
}

func htmlSVGLegendAt(b *strings.Builder, items []htmlLegendItem, x, y int) {
	for i, item := range items {
		yy := float64(y + i*20)
		if strings.HasPrefix(item.Class, "bar-") {
			fmt.Fprintf(b, "<rect class=\"%s\" x=\"%d\" y=\"%.1f\" width=\"20\" height=\"8\" rx=\"2\" />\n", item.Class, x, yy-7)
		} else {
			fmt.Fprintf(b, "<line class=\"%s\" x1=\"%d\" y1=\"%.1f\" x2=\"%d\" y2=\"%.1f\" />\n", item.Class, x, yy-4, x+22, yy-4)
		}
		fmt.Fprintf(b, "<text class=\"label\" x=\"%d\" y=\"%.1f\">%s</text>\n", x+30, yy, htmlText(item.Label))
	}
}

func htmlPolyline(points []htmlPoint) string {
	parts := make([]string, 0, len(points))
	for _, p := range points {
		parts = append(parts, fmt.Sprintf("%.1f,%.1f", p.X, p.Y))
	}
	return strings.Join(parts, " ")
}

func htmlText(s string) string {
	return html.EscapeString(s)
}

func htmlDurationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func shortenLabel(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
