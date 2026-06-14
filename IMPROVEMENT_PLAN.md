# fgaperf Improvement Plan (v2)

A prioritized roadmap for the next phase of fgaperf. Written, like its
predecessor, to be picked up incrementally by future working sessions: each item
states the motivation, a design sketch, the files involved, and acceptance
criteria. Items are independent unless noted.

_Drafted 2026-06-13, superseding the v1 plan. **The v1 plan is complete** — all
35 items shipped (one, gRPC, was deferred and is revived here as item 4). The v1
plan's full per-item completion notes live in git history (this file before
2026-06-13); a compact index of what shipped is in [Completed in v1](#completed-in-v1)
at the end. Two prior bug-audit passes (commits `d498d81` and `1102d71`) closed
ten findings; a follow-up review found no further correctness bugs, only the four
small cleanups folded into item 13._

## Orientation for a new session

Architecture (one file per concern, all `package main`):

| File | Role |
|---|---|
| `main.go` | CLI: `inspect`, `setup`, `probe`, `run`, `all`, `cleanup`, `compare`, `plan`, `validate`, `doctor`, `gen-config`; state file lifecycle |
| `config.go` | YAML config schema, defaults (`applyDefaults`), strict validation, flag overrides |
| `model.go` | Parses compiled model JSON; derives assignable relations, CEL-reachable relations (fixpoint), condition param tuple/request split |
| `seed.go` | Generates the cohort-partitioned tuple graph and condition context values; parallel, resumable seeding |
| `probe.go` | Samples candidate checks, executes each once to learn ground truth, resamples to the target allowed/denied mix, writes `corpus.json` |
| `load.go` | Replays the corpus (closed-loop or fixed-rate), background churn, collects samples, computes stats |
| `client.go` | Thin HTTP client for the OpenFGA REST API (deliberately no SDK/middleware); OIDC token source |
| `metrics.go` | Scrapes + diffs OpenFGA's Prometheus endpoint over the measured phase |
| `report.go` | JSON results + findings markdown; timeline, result-set sizes, sweep table |
| `compare.go` | Diffs two results files into a comparison markdown |
| `progress.go` | Live, throttled progress output (suppressed off-TTY) |

Design principles to preserve when making changes (unchanged from v1 — they are
the contract):

1. **Model-driven, model-agnostic.** Everything is derived from the compiled
   model JSON plus config. No hardcoded type or relation names anywhere.
2. **Probe-then-replay.** Expected outcomes are learned empirically (one
   `HIGHER_CONSISTENCY` check per corpus candidate), never predicted
   statically. Intersections, conditions, and contextual tuples make static
   prediction a tar pit.
3. **Thin hot path.** The load loop must stay free of allocation-heavy
   abstractions, SDK middleware, and retries. Anything added between the
   ticker and the request call is something we're measuring by accident.
4. **Determinism.** A fixed `random_seed` must keep regenerating the same
   world and corpus. `probe` reconstructs the instance space by re-running the
   generator with the same seed — changes to RNG consumption order in `seed.go`
   are breaking changes.
5. **Defaults always work.** An empty config against any compiled model must
   produce a working run (`config.applyDefaults`, CI integration job).

Verification commands: `go vet ./... && go test ./...` (CI also runs `-race`),
then the end-to-end run against `examples/config.yaml` with a local OpenFGA.

---

## P0 — Foundations that unlock the rest ✅

These are leverage points: later items in every theme build on them. Do them
first.

### 1. Streaming latency aggregation (bounded-memory percentiles)

Status: **Complete** (2026-06-13) — added a mergeable three-significant-digit
latency digest, streaming load/report aggregates, digest-backed progress
snapshots, bounded timeline/count aggregators, and tests covering digest
tolerance plus `sample_file` streaming without raw in-memory retention.
Verification: `go vet ./...`; `go test -count=1 ./...`; short OpenFGA smoke
against the local compose stack with `examples/config.yaml`, `-warmup 1s`,
`-duration 2s`, `-rate 50`, and `/tmp/fgaperf-smoke-results`.

Motivation: today every measured `Sample` is retained in a slice and
percentiles are computed by sorting it (`summarizeBy`). At high throughput a
short run accumulates millions of samples — hundreds of MB resident — and the
progress goroutine re-copies and re-sorts the entire slice every 5s under the
collector's lock (the one scalability cost the recent review flagged). This caps
both achievable load and run length, and blocks the soak/distributed items
below.

Design sketch: introduce a merge-able online sketch — an HDR histogram (fixed
relative error, e.g. 3 significant figures) or a t-digest — in a new `digest.go`.
Each reported population (overall, by-target, conditioned/unconditioned,
contextual/without, timeline bucket) holds one digest fed as samples complete.
`Summarize`/`summarizeBy` gain a digest-backed path that fills the existing
`Stats` shape; `loadProgress` reads a digest snapshot instead of copying the
slice. Keep full raw retention only when the operator opts in — the existing
`load.sample_file` already streams raw samples to disk for anyone who needs them.
Preserve determinism: digests are order-insensitive, so merge results are stable.

Files: new `digest.go`; `load.go` (Sample collection, `Summarize`/`summarizeBy`,
the RunLoad collector loop); `progress.go` (`loadProgress.add`/`run`);
`report.go` (`Stats`, `buildTimeline`, `summarizeCounts` stays as-is for counts).

Acceptance: a 60s high-rate closed-loop run keeps resident memory roughly flat
regardless of sample count; reported p50/p90/p95/p99 fall within the digest's
stated tolerance of the exact sort-based values on a fixed seed (add a test that
compares digest vs. exact on a known distribution); progress ticks no longer copy
the sample slice; `sample_file` export still works. **Folds in review
observation #1.**

### 2. Result baseline + regression substrate ✅

Status: **Complete** (2026-06-13) — added compact baseline JSON export via
`fgaperf baseline save <results.json>`, config fingerprints/random seed capture,
and `fgaperf compare -against-baseline <baseline.json> <results.json>` with
percent regression gates (`p99=10%,throughput=-5%` by default). Comparison
caveats/config drift are warnings; threshold breaches return non-zero and name
the regressed metric/target. Verification: `go vet ./...`;
`go test -count=1 ./...`.

Motivation: each run emits a results JSON, but nothing pins a baseline or gates a
regression. A baseline format plus a comparison-against-baseline command is the
substrate both the CI gate (item 7) and trend/significance work (item 10) need.

Design sketch: define a compact baseline JSON — key percentiles per target and
overall, throughput, DS-queries/request, plus the config fingerprint and
`random_seed`. Add `fgaperf baseline save <results.json>` and
`fgaperf compare --against-baseline <baseline.json> [--max-regression p99=10%,throughput=-5%]`,
reusing the existing `compare` diff machinery and `comparabilityCaveats`. The
compare command exits non-zero when any tracked metric regresses past its
threshold.

Files: `compare.go` (reuse `diffConfigs`/`comparabilityCaveats`); `main.go`
(subcommand/flags); new `baseline.go`.

Acceptance: a results file that regresses p99 beyond the threshold exits non-zero
naming the metric and target; an in-tolerance file exits zero; `comparabilityCaveats`
still fire (and downgrade to a warning) when configs differ.

---

## P1 — High-value capability

### 3. Open-model (Poisson) arrival process for fixed-rate ✅

Status: **Complete** (2026-06-13) — added `load.arrival: uniform|poisson`
(default `uniform`, validated). For `poisson` the rate goroutine draws
exponential inter-arrivals (`-ln(1-U)/rate`) from a dedicated seeded RNG
(`random_seed + 1000000007`, an independent stream from the worker/churn RNGs)
and accumulates `intended` times; `DroppedSlots`/`RespLatency` accounting is
untouched (both key off `intended`). The arrival model is recorded in the
embedded resolved config and surfaced on the run-config table for poisson runs;
uniform output is byte-identical. Factored the schedule into a testable
`arrivalGen` (`load.go`). Verification: `go vet ./...`; `go test -race`
(`arrival_test.go`: uniform = even ticker, poisson deterministic + monotonic +
mean rate within 3% of offered + differs from uniform); end-to-end against the
local compose stack at 800 req/s — poisson response-latency p99 6.16ms vs
uniform 3.60ms (visibly heavier tail), achieved 809 vs 800 req/s offered.

Motivation: fixed-rate mode fires slots on a uniform ticker (slot N at
`start + N*interval`). Real traffic is bursty; a perfectly even arrival rate
understates queueing and tail latency. An open-model Poisson process
(exponential inter-arrivals) is the standard production-load model, and the
coordinated-omission groundwork already in place (intended-time accounting, now
warmup-gated) supports it directly.

Design sketch: add `load.arrival: uniform|poisson` (default `uniform` =
today's behavior, byte-for-byte). For `poisson`, the rate generator draws
inter-arrival `= -ln(1-U)/rate` from a dedicated seeded RNG and accumulates
`intended` times from those draws. `DroppedSlots` and `RespLatency` accounting
are unchanged — both already key off `intended`. Determinism holds via the
seeded RNG.

Files: `load.go` (rate-generator goroutine); `config.go` (default + validation);
`report.go` (record the arrival model in the config snapshot it already embeds).

Acceptance: with `arrival: poisson` and a fixed seed, the `intended` series is
deterministic; mean achieved rate ≈ offered; the response-latency tail is
visibly heavier than `uniform` at the same offered rate against the same server;
`uniform` output is unchanged.

### 4. gRPC client option (revives deferred v1 item 18) ✅

Status: **Complete** (2026-06-14) — added `load.transport: http|grpc` (default
`http`, validated; `-transport` flag override). Extracted a small `LoadClient`
interface (the four read endpoints plus the churn write/delete) that both the
HTTP `FGAClient` and a new `GRPCClient` (`client_grpc.go`) satisfy;
`RunLoad`/`RunSweep`/`runChurn`/the `do*` helpers take the interface. The gRPC
client is a single tuned `grpc.ClientConn` (generous HTTP/2 windows, no
interceptors; pre-shared/OIDC auth rides as request metadata) against the pinned
`github.com/openfga/api` stubs; setup and probe stay on HTTP (principle #3
preserved). gRPC dials `openfga.grpc_url` (default `host:8081` derived from
`api_url`; `openfga.grpc_tls` for managed/cloud). The transport is recorded in
`LoadResult`/`Report` and surfaced on the run-config table for gRPC runs; HTTP
output is byte-identical (top-level field is `omitempty`). Verification:
`go vet ./...`; `go test -race` (`client_grpc_test.go`: an in-process fake gRPC
server drives RunLoad over all four endpoints with zero transport errors,
write/delete round-trips, context + contextual-tuple + CEL-condition propagation
through `structpb`, grpc_url derivation, and gRPC-status → error-class mapping);
end-to-end against a local OpenFGA (HTTP :8080 + gRPC :8081) — gRPC lower at
every percentile than HTTP for the same closed-loop check workload (p50 1.52 vs
1.74 ms, p95 3.18 vs 3.64 ms, p99 4.49 vs 4.81 ms; 9642 vs 8380 checks/s), all
four endpoints error-free over gRPC. CI publishes :8081 and runs a gRPC smoke.

Motivation: OpenFGA's gRPC API is the lower-overhead production path; the HTTP +
JSON overhead is baked into today's client-side numbers. High-throughput callers
use gRPC, and "what does the server actually cost over gRPC" is unanswerable
today. Deferred in v1 to avoid an SDK dependency on the hot path; worth doing now
that the client seam is stable.

Design sketch: extract the small interface the load loop needs from `FGAClient`
(`Check`, `BatchCheck`, `ListObjects`, `ListUsers`) and add a `transport:
http|grpc` knob selecting an alternate implementation. Keep principle #3: a
single tuned `grpc.ClientConn`, no interceptors, pinned/vendored stubs. Seeding
and probe can stay on HTTP; only the load loop needs the gRPC path.

Files: `client.go` (extract the load-loop interface); new `client_grpc.go`;
`config.go` (`transport`); `load.go` (transport selection); CI workflow (gRPC
smoke).

Acceptance: `transport: grpc` runs all four endpoints against a gRPC OpenFGA and
reports lower per-request overhead than HTTP for the same workload; HTTP stays
the default and unchanged; CI exercises a gRPC smoke run.

### 5. Traffic replay: build the corpus from a real check log ✅

Status: **Complete** (2026-06-14) — added `corpus_source: probe|replay` (default
`probe`, validated; `replay` requires `replay.file`). New `replay.go` parses a
JSONL check log (`{user, relation, object[, contextual_tuples, context]}`, extra
fields ignored so a raw request log feeds in directly), deduplicates entries,
and tallies natural per-target counts. The shared ground-truth executor was
extracted from `BuildCorpus` into `classifyCandidates` (one
`HIGHER_CONSISTENCY` check per distinct entry, principle #2 preserved); both the
synthesized and replay builders use it. `BuildReplayCorpus` sets the corpus
`Weights` to the log's per-target counts, so the existing `corpusPicker`
reproduces the log's target mix (within-target picks stay uniform over distinct
checks); probe sampling/resampling and the `contextual` block are bypassed under
replay. Malformed/blank lines are counted, reported (with sample reasons), and
skipped — never fatal. `probe` (and `all`) branch on `corpus_source`; `run` is
unchanged. Verification: `go vet ./...`; `go test -race` (`replay_test.go`:
dedup + natural-count weights, context/contextual tagging, malformed-line
skipping, unknown-field tolerance, weights-drive-target-mix via `corpusPicker`,
config validation, and an httptest end-to-end `BuildReplayCorpus` asserting
distinct-only `HIGHER_CONSISTENCY` checks, recorded ground truth, weights, and
churn templates); end-to-end against the local compose stack — seeded a store,
derived a deliberately viewer-skewed 1165-line log (2 malformed lines) from a
normal probe corpus, replayed it: 2 lines skipped/reported, 681 distinct checks
across 5 targets, 0 errors / 0 mismatches, and the load's per-target sample mix
tracked the log's target distribution within ~0.5% (viewer 51.4% run vs 51.9%
log). Docs: README config table + How-It-Works + glossary, configuration-
reference `replay` section, a recipes entry, and a commented block in
`examples/config.yaml`.

Motivation: the probe synthesizes a check distribution from the model. Teams that
have a real check log (OpenFGA request logs, app audit trails) want to replay
*that* exact distribution so the load mix matches production and probe synthesis
is bypassed.

Design sketch: add `corpus_source: probe|replay` with `replay.file` pointing at a
JSONL of `{user, relation, object[, contextual_tuples, context]}`. fgaperf still
executes each distinct entry once at `HIGHER_CONSISTENCY` to learn ground truth
(principle #2 preserved), then replays under load with frequency weighting drawn
from the log's natural counts. Reuse `corpusPicker` weighting.

Files: `probe.go` (alternate corpus builder); `config.go`; new `replay.go`.

Acceptance: given a JSONL log against a seeded store, fgaperf builds a corpus
whose target mix matches the log, verifies ground truth, and runs load;
malformed lines are reported and skipped, not fatal.

### 6. Per-relation datastore-query attribution ✅

Status: **Complete** (2026-06-14) — added `probe.attribute_ds_queries` (off by
default; validation requires `metrics.prometheus_url`). After the corpus is
built, `attributeDatastoreQueries` (`probe.go`) replays a small distinct batch
per target — one relation at a time, at `HIGHER_CONSISTENCY` so checks bypass
the check cache — and diffs OpenFGA's `openfga_datastore_query_count` histogram
around each batch via `dsQueryDiff` (`metrics.go`), recording per-target mean
datastore queries/check on the corpus (`Corpus.DSQueries`, `omitempty`). The
pass runs after `BuildCorpus`/`BuildReplayCorpus` and does not consume the
generator RNG, so corpus entries stay deterministic; only the measured values
vary. It is best-effort: a failed scrape or a target with no histogram movement
is left unattributed, never fatal. The report carries it through
(`Report.DSQueriesByTarget`, `omitempty`) and the per-relation table gains a "DS
queries/check (probe)" column — omitted for `batch-check` (rows mix relations)
and whenever the pass did not run. When off, the corpus, results JSON, and
per-relation table are byte-identical (the golden's only change is one legend
sentence). Verification: `go vet ./...`; `go test -race -count=1 ./...`
(`metrics_test.go`: `dsQueryDiff` sum/count + missing-family safety;
`probe_test.go`: an httptest server serving `/check` + `/metrics` with
per-relation costs drives `attributeDatastoreQueries`, asserting per-relation
attribution, deep > direct ordering, distinct-only dedup, and best-effort no-op
on a 500ing metrics endpoint; `config_test.go`: validation rejects the flag
without a metrics URL and accepts it with one; `report_test.go`: the DS column
renders only with data and is omitted for batch-check); end-to-end against the
local compose stack with attribution on — attributed 5/5 targets with sane
values (`group#member` direct = 1.0, `document#editor` medium = 5.7, the deep
tuple-to-userset/recursive paths `document#viewer` 14.6, `folder#viewer` 11.3,
`document#can_share` 10.9). Docs: README config table + findings-sections table,
configuration-reference subsection, a recipes entry under "Spot a Hot Relation",
and commented knobs in `examples/config.yaml` and `gen-config` output.

Motivation: the server-side view is aggregate over the whole measured phase —
mean DS-queries/request for the run, not per relation. The per-relation cost
("this relation costs ~12 datastore reads, that one costs 2") is the sharpest
capacity signal for spotting an expensive rewrite, and it's invisible today.

Design sketch: at probe time, at low concurrency and one relation at a time, diff
the Prometheus `openfga_datastore_query_count` histogram around a small batch of
checks per target to attribute approximate DS-queries/check per relation. Record
it on the corpus and surface a per-relation column in the report. Best-effort and
gated on a metrics endpoint; off by default so the normal probe path is
unchanged.

Files: `metrics.go` (a targeted around-a-batch diff helper); `probe.go`
(attribution pass behind a flag); `report.go` (per-relation DS column).

Acceptance: with metrics configured and the flag on, the per-relation table gains
a "DS queries/check (probe)" column whose values are sane (a deep tuple-to-userset
relation > a direct relation); without metrics the column is omitted; the default
probe path is byte-identical when the flag is off.

### 7. CI perf-regression gate (built on item 2) ✅

Status: **Complete** (2026-06-14) — item 2 already made
`compare -against-baseline` exit non-zero on a threshold breach; item 7 makes
that an actual gate. Added an `-exit-on-regression` knob (default `true`,
preserving item 2's shipped behavior; `-exit-on-regression=false` reports
breaches as warnings + markdown but exits zero for an advisory/non-blocking
comparison) threaded through `compareAgainstBaseline`. Added a live `perf-gate`
job to `.github/workflows/ci.yaml` that runs the pipeline against a throwaway
OpenFGA, saves a baseline, confirms the gate **passes** the healthy run, then
forges an unbeatable baseline (1 ns p99, absurd throughput via `jq`) and
confirms the gate **fails** the now-regressed run — the executable acceptance
test. Documented end to end in a new `docs/ci-regression-gate.md` (copy-paste
GitHub Actions recipe, threshold reference, baseline-refresh workflow, advisory
mode), with a README "Regression gating" subsection, a recipes entry, and a
configuration-reference subsection. Verification: `go vet ./...`;
`go test -race ./...` (`baseline_test.go` gains an advisory-mode assertion: the
same breaching results exit zero with `-exit-on-regression=false` and still
write the comparison artifact); end-to-end against a local OpenFGA — gate passed
a healthy run (exit 0), failed a forged 1 ns-p99 baseline (exit 1, naming
`p99 overall` + `throughput overall`), and advisory mode reported both breaches
while exiting 0.

Motivation: catch performance regressions from OpenFGA upgrades, datastore tuning,
or model changes automatically, the same way the unit tests catch correctness
regressions.

Design sketch: a documented CI recipe plus `compare --against-baseline` from item
2 — store a baseline artifact, run `fgaperf all`, compare, fail the job on
regression. Provide a copy-paste GitHub Actions snippet in `docs/`. A small
`--exit-on-regression` convenience can wrap the threshold check.

Files: `docs/` (CI recipe); reuse item 2; minor `main.go`.

Acceptance: a CI job fails on an injected regression and passes otherwise;
documented end to end against the compose stack.

---

## P2 — Breadth and polish

### 8. Distributed / multi-client load generation ✅

Motivation: a single fgaperf process is bounded by one host's CPU and NIC;
saturating a large OpenFGA cluster needs several coordinated load generators. The
merge-able digest from item 1 makes aggregation clean.

Design sketch: simplest first — each process runs the same corpus and store with
a distinct worker-id RNG offset and emits its own results JSON containing the
serialized digests (item 1); a new `fgaperf merge results-*.json` combines the
digests into one coherent report (throughput summed, percentiles merged). A
coordinator/agent mode can come later if needed.

Files: new `merge` subcommand; reuse `digest.go`; `report.go` (merge path).

Implemented 2026-06-14: results JSON now embeds mergeable digest sketches for
the measured latency populations, per-target splits, result counts, timeline,
and write churn. `load.client_id` / `-client-id` offsets the load RNG streams
for multi-client runs, and the new `fgaperf merge` subcommand combines
single-rate result files into one report (summing concurrency, offered/achieved
rates, throughput, errors, and mismatches while merging percentile sketches).
Docs now cover the workflow in README, the configuration reference, methodology,
recipes, and generated/example configs.

Acceptance evidence: `go test ./...` includes synthetic two-client merge tests
that verify summed throughput/rates and merged p50/p99 distributions from the
digests, plus compatibility rejection and CLI no-server coverage.

### 9. Soak mode: long-run stability with interim reports

Motivation: hours-long runs surface leaks, cache-eviction cliffs, and datastore
compaction that a 60s run misses. Today's retain-all-samples model can't sustain
that (item 1 fixes it) and there is no interim reporting.

Design sketch: with item 1's streaming digests, add `load.report_interval`; emit
a rolling interim findings snapshot at that cadence and rotate the `sample_file`.
Extend `timelineWidth`'s bucket ladder so multi-hour windows bucket by the minute
or five.

Files: `load.go` (interim emit, file rotation); `report.go` (`timelineWidth`).

Acceptance: a multi-minute run emits interim reports at the configured cadence
with flat memory; the final report covers the whole window; timeline rows stay
~12 regardless of duration.

Implemented 2026-06-14: added `load.report_interval` for long single-rate
soaks. During the measured phase, `RunLoad` emits cumulative
`interim-results-*` / `interim-findings-*` snapshots from the streaming digest
aggregates at the configured cadence, while the normal final report remains the
whole-window source of truth. When `load.sample_file` is set, the first interval
uses the configured path and subsequent intervals rotate to numbered chunks
(`samples-002.jsonl[.gz]`, ...). `load.report_interval` is rejected with
`load.sweep` to avoid mixing sweep append semantics with rotation. Timeline
bucket widths now widen through multi-minute/hour buckets so multi-hour soak
reports stay roughly a dozen rows.

Acceptance evidence: `go test ./...` (with `GOCACHE=/tmp/fgaperf-go-build`)
covers config validation, timeline width expansion, bounded timeline
accumulator buckets for a two-hour window, sample segment naming, and a short
local load run that writes interim reports, rotates samples, keeps raw samples
out of memory, and preserves the aggregate sample count across chunks.

### 10. Statistical significance in `compare`

Motivation: a p99 delta between two single runs may be noise. Today `compare`
reports the delta with no sense of whether it's meaningful.

Design sketch: add a `repeat: N` run mode producing N results, and have `compare`
(or a `summarize`) report mean ± stdev per metric and label a delta "significant"
only when it exceeds observed run-to-run variance. Pairs naturally with reporting
percentile confidence from the repeated runs.

Files: `main.go` (repeat mode); `compare.go`; `report.go`.

Acceptance: comparing two sets of repeated runs labels each metric delta as
"significant" or "within noise" from the observed variance.

### 11. HTML / visual report

Motivation: the markdown findings are ideal for terminals and PRs, but a
self-contained HTML with charts (sweep curve, latency-over-time, distribution
CDF) is far more digestible for stakeholders and capacity reviews.

Design sketch: emit an optional `report.html` alongside the JSON/MD — a single
self-contained file with inline SVG charts generated from the same `Report`
struct, no external assets or network. Headline charts: the sweep knee curve, the
latency timeline, and per-relation latency bars.

Files: new `report_html.go`; `main.go` `run()` (emit alongside, behind a flag or
always).

Acceptance: `fgaperf run` writes a self-contained HTML that renders the sweep
curve, timeline, and per-relation latency offline (no external fetches).

### 12. Mixed-endpoint workloads

Motivation: real services blend `check`, `batch-check`, and list calls; a run is
single-endpoint today, so blended contention is unmeasurable.

Design sketch: let `load.endpoint` also accept a weighted set (e.g.
`check: 70, list-objects: 20, batch-check: 10`); the worker picks an endpoint per
request by weight, reusing the `corpusPicker` weighting pattern. The report splits
percentiles per endpoint.

Files: `config.go` (parse + validate the weighted form); `load.go` (per-request
dispatch); `report.go` (per-endpoint split).

Acceptance: a mixed-endpoint config exercises all selected endpoints at the
configured shares and reports per-endpoint percentiles; single-endpoint behavior
is unchanged.

### 13. Review cleanups (small, independent)

Carried over from the post-audit review — none are correctness bugs, all are
low-risk hygiene.

- **C1. UTF-8-safe `truncate`** (`client.go`) — truncating an error body at a
  byte boundary can emit an invalid-UTF-8 tail into `ErrorSamples`. Trim on a
  rune boundary (`utf8.DecodeLastRuneInString` back-off). _Review observation #2._
- **C2. Guard `runAll` store deletion with `sync.Once`** (`main.go`) — a SIGINT
  landing during the normal-exit `deleteStore` defer can double-call
  `DeleteStore` (harmless 404 warning) and races `os.Exit(1)`. A `sync.Once`
  around `deleteStore` tidies it. _Review observation #3._
- **C3.** (Subsumed by item 1 — the progress re-summarize cost. _Review
  observation #1._)
- **C4. Document the metrics counter-reset clamp** (`metrics.go`) — `max(0, …)`
  per bucket can break histogram monotonicity after a mid-run counter reset
  (server restart), where results are already invalid. A one-line comment or a
  "counter reset detected; server-side view suppressed" guard. _Review
  observation #4._

Acceptance: `truncate` never emits a partial rune (unit test with multi-byte
input); a simulated interrupt during `runAll` cleanup deletes the store exactly
once; metrics behavior on counter reset is documented or guarded.

---

## Suggested sequencing

1. **Unlock scale and fidelity first:** item 1 (streaming digests) — it removes
   the memory ceiling and the progress cost, and is a prerequisite for items 8
   and 9.
2. **Build the regression substrate:** item 2, then item 7 — baseline compare,
   then the CI gate that consumes it.
3. **Realism, by demand:** items 3 (Poisson), 5 (replay), and 4 (gRPC) in
   whatever order matches the questions you actually need to answer; 6
   (per-relation DS attribution) whenever a capacity question gets specific.
4. **Breadth:** items 8–12 opportunistically. Item 8 (distributed) and 9 (soak)
   both lean on item 1. Item 11 (HTML) is independent. Item 10 (significance)
   wants a repeat-run mode.
5. **Cleanups:** item 13 anytime; C1/C2 are a few lines each.

## Invariants to re-verify after any change

- `go vet ./... && go test ./...` clean; CI's `-race` job clean.
- `./fgaperf all` with the example config against a fresh local OpenFGA
  completes, deletes its store, and writes both result files.
- Same `random_seed` + same config ⇒ identical generated tuples and corpus
  candidates. Determinism is part of the contract; if generation code is
  touched, add/keep a regression test that hashes generated tuples for a fixed
  config.
- No type, relation, right, or tenant names from any private model appear in
  code, examples, docs, or this file. Private inputs live only in gitignored
  paths (`main-model.json`, `config.yaml`, `models/`, `tests/`).

## Completed in v1

All shipped and verified (full per-item notes in git history; ✅ in the prior
plan). Listed so this file stays a complete record of scope.

**P0 — Measurement correctness:** (1) fixed-rate coordinated-omission correction,
(2) corpus duplication reporting + bound, (3) measured-wall-clock throughput,
(4) error visibility, (5) strict config validation.

**P1 — Capability:** (6) server-side Prometheus metrics, (7) rate-sweep knee
detection, (8) `compare` A/B reporting, (9) embedded resolved config + environment
in results, (10) mixed read/write (background churn), (11) per-target workload
weights, (12) richer generation shapes (per-user-type fanout, value
distributions), (13) mismatch diagnostics.

**P2 — Breadth & ergonomics:** (14) `list-objects`/`list-users` endpoints,
(15) CLI flag overrides, (16) latency timeline, (17) raw sample export,
(18) gRPC — _deferred; revived as item 4 above_, (19) OIDC auth, (20) seeding at
scale, (21) CI hardening, (22) methodology docs.

**P2 — User-friendliness & onboarding:** (23) live progress, (24) `doctor`
pre-flight, (25) actionable error wrapping, (26) `plan` server-free dry run,
(27) findings TL;DR headline, (28) "what you might change" hints, (29) ANSI
color, (30) getting-started walkthrough, (31) recipes, (32) per-subcommand help,
(33) README badges, (34) troubleshooting docs, (35) `inspect --json`.
