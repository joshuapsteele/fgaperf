# fgaperf Improvement Plan

A prioritized roadmap for making fgaperf more valuable and robust as an OpenFGA
performance-testing tool. Written to be picked up incrementally by future
working sessions: each item states the motivation, a design sketch, the files
involved, and acceptance criteria. Items are independent unless noted.

## Orientation for a new session

Architecture (one file per concern, all `package main`):

| File | Role |
|---|---|
| `main.go` | CLI: `inspect`, `setup`, `probe`, `run`, `all`, `cleanup`; state file lifecycle |
| `config.go` | YAML config schema and defaults |
| `model.go` | Parses compiled model JSON; derives assignable relations, CEL-reachable relations, condition param split |
| `seed.go` | Generates the cohort-partitioned tuple graph and condition context values; parallel seeding |
| `probe.go` | Samples candidate checks, executes each once to learn ground truth, resamples to the target allowed/denied mix, writes `corpus.json` |
| `load.go` | Replays the corpus (closed-loop or fixed-rate), collects samples, computes stats |
| `client.go` | Thin HTTP client for the OpenFGA REST API (deliberately no SDK/middleware) |
| `report.go` | JSON results + findings markdown |

Design principles to preserve when making changes:

1. **Model-driven, model-agnostic.** Everything is derived from the compiled
   model JSON plus config. No hardcoded type or relation names anywhere.
2. **Probe-then-replay.** Expected outcomes are learned empirically (one
   `HIGHER_CONSISTENCY` check per corpus candidate), never predicted
   statically. Don't add static outcome prediction; intersections, conditions,
   and contextual tuples make it a tar pit.
3. **Thin hot path.** The load loop must stay free of allocation-heavy
   abstractions, SDK middleware, and retries. Anything added between the
   ticker and `http.Client.Do` is something we're measuring by accident.
4. **Determinism.** A fixed `random_seed` must keep regenerating the same
   world. `probe` reconstructs the instance space by re-running the generator
   with the same seed — changes to generation order are breaking changes.
5. **Defaults always work.** An empty config against any compiled model must
   produce a working run (`config.applyDefaults`, CI integration job).

Verification commands: `go vet ./... && go test ./...`, then the CI-style
end-to-end run (see `.github/workflows/ci.yaml` integration job) against
`examples/config.yaml` with a local OpenFGA.

---

## P0 — Measurement correctness

These affect whether the numbers can be trusted. Do these before adding
features.

**Status: all five P0 items done (2026-06-12).** Verified with `go vet`,
`go test -race`, and end-to-end runs against the compose stack: a closed-loop
smoke run, plus an oversubscribed fixed-rate run (6000 req/s offered, 4
workers) that correctly reported achieved 3768 req/s, 16.9k dropped slots, and
response-latency p99 of 1622 ms vs service p99 of 2.96 ms. Per-item notes
below.

### 1. Fix coordinated omission in fixed-rate mode ✅

**Problem.** In `load.go`, the rate ticker drops slots when the channel is
full ("drop the slot rather than build backlog"), and latency is measured from
the moment a worker *starts* the request, not from when the request *should
have* started. Under saturation this silently lowers the offered rate and
hides queueing delay — the classic coordinated-omission trap. The report
prints the configured rate, not the achieved rate, so the reader can't even
tell it happened.

**Sketch.**
- Track intended send times: slot N's intended time is `start + N*interval`.
  Record both service latency (current measurement) and response latency
  (`completion - intended_send`), reporting both.
- Count dropped/late slots and report achieved rate vs offered rate in the
  findings doc, with a loud callout when achieved < ~98% of offered.
- Keep closed-loop mode as-is (it has no offered rate to fall behind).

**Files.** `load.go` (worker loop, `Sample`, `LoadResult`), `report.go`.

**Accept.** A fixed-rate run against a deliberately undersized server reports
achieved rate < offered rate and shows response-latency percentiles much worse
than service-latency percentiles, instead of silently looking healthy.

**Done.** The rate dispatcher now sends *intended* times (slot N fires at
`start + N*interval`) through `rateCh`; workers record
`RespLatency = completion - intended`. The buffer holds one second of slots and
drops (counted in `DroppedSlots`) only when workers fall a full second behind.
Report gains `achieved_rate_per_sec`, `dropped_rate_slots`, a
`response_latency` stats block, and a loud markdown callout when achieved <
98% of offered. Closed-loop mode unchanged. Verified per the accept criterion
(see status note above).

### 2. Report corpus duplication, and bound it ✅

**Problem.** `probe.go` `resample`/`sampleN` draw with replacement to hit
`allowed_ratio`. When natural allowed (or denied) outcomes are scarce, the
corpus becomes a few distinct entries repeated many times. Replaying
near-identical checks inflates server cache hit rates and understates latency.
Nothing in the output reveals this.

**Sketch.**
- Track distinct-entry counts per target through resampling; print them in
  `probe` output and persist them in `corpus.json`.
- Add `probe.max_duplication` (default e.g. 5×): if hitting `allowed_ratio`
  would need more duplication than that, keep the natural mix for that target
  and warn — same fallback the code already uses for single-class targets.
- Surface corpus uniqueness in the report's caveats section (the data is in
  the corpus; `report.go` already receives it).

**Files.** `probe.go`, `report.go`, `config.go`.

**Accept.** A run with a very low natural allowed rate prints a duplication
warning, and the findings doc states distinct-vs-total corpus entries.

**Done.** `probe.max_duplication` (default 5, -1 = unbounded) makes `resample`
keep the natural mix with a warning when hitting `allowed_ratio` would exceed
the cap (`TestResampleCapsDuplication`). `Corpus` carries per-target
`target_stats` (total/distinct) in `corpus.json`; probe prints overall distinct
count and warns per target above 2x duplication; the findings doc states
distinct-vs-total in the config table and a caveats paragraph. Observed live:
the example model's `document#can_share` (natural 2 allowed / 198 denied) now
keeps its natural mix instead of 50 copies of 2 checks.

### 3. Compute throughput over the measured wall-clock window ✅

**Problem.** `report.go` divides items by the *configured* duration. Workers
checking `time.Now().Before(deadline)` before each request means in-flight
requests complete after the deadline; slow tails stretch the real window.

**Sketch.** Record first-sample and last-sample completion timestamps in the
measured phase and divide by that window. Keep configured duration in the
config table.

**Files.** `load.go`, `report.go`.

**Accept.** Throughput for a run with multi-second p99s no longer exceeds what
the wall clock supports.

**Done.** Every `Sample` records its completion timestamp; the collector tracks
first/last measured completion into `LoadResult.MeasuredWindow`, and the report
divides items by that window (falling back to configured duration only when no
samples exist). The findings doc shows the actual window next to the configured
duration; `measured_window` is in the results JSON.

### 4. Error visibility ✅

**Problem.** Errors are a single counter. A run with 5% timeouts and a run
with 5% HTTP 429s look identical, and the error rate isn't broken down per
target. `client.go` returns rich error strings but `load.go` flattens them to
`Err bool`.

**Sketch.** Classify into a small enum (timeout, connection, 4xx, 5xx,
decode) on the sample; report a per-class count and a per-target error column.
Keep the first N error strings verbatim for the findings doc, like probe
already does for its errors.

**Files.** `client.go` (typed error or status code on the error), `load.go`,
`report.go`.

**Accept.** A run against a server that starts returning 429s mid-run produces
a findings doc that says so.

**Done.** `client.go` returns a typed `*HTTPError` carrying the status code;
`classifyErr` in `load.go` buckets errors into timeout / connection / 4xx /
5xx / decode (unit-tested in `load_test.go`). Samples carry class + verbatim
message; the report gets `errors_by_class`, the first 5 verbatim
`error_samples` (so a 429 burst is named explicitly in the findings doc), an
"Errors" markdown section, and an Errors column in the per-relation table.

### 5. Validate config strictly ✅

**Problem.** `yaml.Unmarshal` ignores unknown keys, so a typo like
`alowed_ratio:` silently runs with defaults. For a measurement tool,
misconfiguration that looks like configuration is data corruption.

**Sketch.** Use `yaml.Decoder` with `KnownFields(true)` in
`LoadConfigFile`. Additionally validate cross-field invariants at load time:
consistency is one of the two legal values, endpoint is `check|batch-check`,
probabilities are in [0,1], pools referenced by `conditions` exist, fanout and
contextual keys parse as `type#relation`. After the model loads, also verify
fanout/instances/contextual/probe-target keys exist in the model (today a
misspelled relation in `probe.targets` errors only as an empty corpus, and a
misspelled fanout key silently uses the default).

**Files.** `config.go`, `main.go` (post-model validation pass), tests in
`config_test.go`.

**Accept.** A config with an unknown key or a fanout key naming a nonexistent
relation fails fast with a message naming the bad key.

**Done.** `LoadConfigFile` decodes with `KnownFields(true)` and then runs
`Config.validate()` (consistency/endpoint enums, probabilities in [0,1],
`type#relation` key shapes, pools referenced by conditions exist, positive
counts). `Config.validateAgainstModel()` runs in `main.go` for
inspect/setup/probe/all and checks that fanout, instances, contextual,
probe-target, subject-type, and condition keys all exist in the model. Both the
example and the private config pass; bad configs covered in `config_test.go`.

---

## P1 — High-value capability gaps

### 6. Capture server-side metrics alongside client-side latency ✅

**Motivation.** This is the single most valuable OpenFGA-specific addition.
Client-side latency conflates network, JSON, and server work. OpenFGA exposes
Prometheus metrics (the bundled compose already publishes `:2112` and enables
datastore metrics) including request duration, **datastore query count per
request**, and dispatch counts — datastore queries per check is *the* capacity
currency for OpenFGA sizing, and no client-side number can substitute for it.

**Sketch.**
- New optional config: `metrics.prometheus_url`. When set, scrape `/metrics`
  at measured-phase start and end; diff counters/histograms.
- Report per-run: server-side request duration percentiles (from histogram
  buckets), total datastore queries, datastore queries per check, cache
  hit/miss counters when the query cache is enabled.
- Plain HTTP GET + text-format parsing of the handful of metric families
  needed; no Prometheus client dependency on the hot path (scrapes happen
  outside the measurement window).

**Files.** New `metrics.go`, `config.go`, `main.go` (`run`), `report.go`.

**Accept.** Against the bundled compose stack, the findings doc includes a
"Server-side view" section with datastore queries per check; the tool still
works (section omitted) when the URL is unset or unreachable.

**Done (2026-06-12).** New `metrics.go` with a minimal Prometheus text parser
(no client dependency): snapshots at the warmup/measured boundary (background
goroutine in `RunLoad`) and after the last worker, diffed with counter-reset
clamping. Families: `openfga_request_duration_ms`,
`openfga_datastore_query_count`, `openfga_dispatch_count` (histograms,
aggregated across label sets — the diff window isolates load traffic) and the
check-cache counters. Percentiles are bucket-interpolated à la
`histogram_quantile`. `metrics.prometheus_url` is set in
`examples/config.yaml`; scrape failures warn and skip the section. Verified
against the compose stack: findings doc reported 9.74 datastore queries per
request, server-side p99 7.57 ms. Parser/diff/quantile unit tests in
`metrics_test.go`.

### 7. Rate sweep: find the saturation knee automatically ✅

**Motivation.** One closed-loop point and one fixed-rate point don't answer
the question users actually have: "what throughput can this deployment sustain
within an SLO?" Today that means many manual runs and hand-built curves.

**Sketch.**
- `load.sweep: { rates: [100, 200, 400, ...], step_duration: 60s }` (explicit
  list first; auto-stepping can come later). Each step reuses the same corpus
  and store; warmup once.
- Emit one stats block per step plus a throughput-vs-p99 table in the findings
  doc; mark the highest step where achieved rate ≈ offered rate and p99 is
  under an optional `load.slo_p99`.
- Depends on item 1 (achieved-rate reporting) to know when a step saturated.

**Files.** `load.go`, `config.go`, `report.go`, `main.go`.

**Accept.** A single `fgaperf run` with a sweep config produces a findings doc
with a rate/latency table from which the knee is readable, and names the last
non-saturated step.

**Done (2026-06-12).** `load.sweep: {rates: [...], step_duration: 60s}` plus
optional `load.slo_p99` (checked against response-latency p99 — the
caller-experienced number). `RunSweep` reuses corpus and store; warmup runs
only before the first step. The findings doc gains a "Rate sweep" table
(offered/achieved/dropped/p50/p95/p99/response-p99/errors/datastore-queries)
with the knee marked; the headline sections reflect the knee step (explicitly
labeled), or the final step when everything saturated. Knee = highest step
with achieved ≥ 98% of offered and SLO passing. Verified live: rates
[1000, 3000, 8000] against the compose stack found the knee at 3000 (8000
achieved only ~4900). Unit tests cover knee selection and the all-saturated
case.

### 8. `fgaperf compare`: first-class A/B reporting ✅

**Motivation.** The tool's natural use is comparative: consistency modes,
cache on/off, model variants, OpenFGA versions, instance counts. The JSON
results exist, but comparison is currently manual.

**Sketch.** `fgaperf compare results-a.json results-b.json [-o dir]` →
markdown with side-by-side overall and per-target percentiles, deltas (ms and
%), and config differences pulled from the embedded config snapshots (item 9).
Refuse (or loudly caveat) comparisons where corpus size, endpoint, or
duration differ.

**Files.** New `compare.go`, `main.go`.

**Accept.** Two runs differing only in `load.consistency` produce a compare
doc showing per-relation deltas and naming the one config difference.

**Done (2026-06-12).** New `compare.go`: `fgaperf compare a.json b.json`
writes `results/compare-<stamp>.md` with side-by-side overall stats (Δ ms and
%), a server-side delta table (datastore queries per request) when both runs
have one, per-relation p50/p99 deltas, and a recursive diff of the embedded
resolved configs. Loud "not directly comparable" caveats when endpoint, corpus
size, duration, concurrency, or tool version differ. Verified live per the
accept criterion. Along the way fixed a reproducibility bug this exposed:
`resample` iterated targets in map order, consuming the RNG differently per
run; it now iterates sorted (regression test `TestResampleDeterministic`).

### 9. Embed the resolved config and environment in results JSON ✅

**Motivation.** Reproducibility. The current `Report` records a handful of
load parameters but not the seed graph shape, fanout, pools, condition
settings, or `random_seed` — the things that determine what was measured.
Results files outlive memory of how they were produced. Prerequisite for a
useful `compare`.

**Sketch.** Add `resolved_config` (the post-defaults `Config`, marshaled) and
an `environment` block (tool version, OpenFGA server version from `GET
/healthz`/build info if available, client GOOS/GOARCH/CPU count — the
markdown already prints some of this) to the results JSON. Consider a
warning note when the working tree is dirty versus the recorded tool version.

**Files.** `report.go`, `main.go`.

**Accept.** A results JSON alone is sufficient to recreate the run: it
contains every knob that affects generation and load.

**Done (2026-06-12).** `Config.Resolved()` round-trips the post-defaults
config through YAML into a generic map (yaml key names and human-readable
durations preserved; `openfga.api_token` redacted when set) and embeds it as
`resolved_config` in the results JSON, alongside an `environment` block
(os/arch/cpus/go version). OpenFGA exposes no version endpoint over HTTP, so
server version is not recorded. Verified in a live run: `random_seed`, seed
shape, pools, and load knobs all present in the output JSON.

### 10. Mixed read/write workloads ✅

**Motivation.** Production OpenFGA serves checks *while* tuples churn, and
writes invalidate caches; a read-only steady state is the server's best case.
Currently writes only happen at seed time.

**Sketch.**
- `load.write_rate: <tuples/sec>` (default 0): a dedicated writer goroutine
  (not the check workers) writes/deletes generated tuples during the measured
  phase — e.g. delete-then-rewrite of randomly chosen seeded tuples so the
  corpus ground truth stays valid, or writes of fresh tuples in instance
  cohorts the corpus never samples (safer; keeps `verify_results` meaningful).
- Report write latency separately and annotate the check stats as
  "with N writes/sec background churn".
- Keep `verify_results` correctness as the constraint that picks the design:
  background churn must not change any corpus entry's expected outcome.

**Files.** `load.go`, `seed.go` (tuple selection for churn), `config.go`,
`report.go`.

**Accept.** A run with churn enabled shows zero verification mismatches and
visibly different check latency vs a churn-free run with caching enabled.

**Done (2026-06-12).** `load.write_rate: <tuples/sec>` (default 0) runs a
dedicated churn goroutine alongside the check workers. The fresh-instances
design won over delete-then-rewrite: `probe.go` collects `ChurnTemplates`
from the model — (object type, relation, user type) shapes whose direct ref
is plain (no userset, no wildcard, no condition) and whose user type is
terminal when any terminal templates exist — and the churn loop instantiates
them with nonce-scoped IDs (`type:churn-<nonce>-<seq>`) that no seeded tuple
or corpus check ever references, so corpus ground truth cannot shift. The
loop keeps a bounded window of 64 live tuples, deleting the oldest once full
so both write and delete invalidation paths churn; `DeleteTuples` strips
conditions (the write endpoint rejects them on deletes). Measured-phase
write/delete latencies are reported as a "Background tuple writes" row in
the findings table, a "Background churn" config row, and an annotation that
check stats include the write traffic; if the model has no safe template the
run warns and disables churn rather than failing. Verified end-to-end:
`write_rate: 50` over an 8s measured window produced 400 write/delete calls,
zero verification mismatches, and write p99 reported alongside check stats;
`go test -race ./...` passes with a unit test asserting templates are plain
and terminal-preferred against the example model.

### 11. Per-target workload weights ✅

**Motivation.** Probe samples every target equally and load replays the
corpus uniformly, so every relation gets the same traffic share. Real traffic
is skewed toward a few entry-point relations. Headline numbers should be
reweightable to a production-like mix.

**Sketch.** `probe.targets` accepts optional weights
(`- relation: type#rel`, `weight: 8`); load picks corpus entries by target
weight instead of uniformly. Per-target stats are unaffected; the *overall*
row becomes mix-weighted. Default weight 1 preserves current behavior.

**Files.** `config.go`, `probe.go` (corpus carries weights), `load.go`.

**Accept.** Doubling one target's weight roughly doubles its request share in
the per-relation table without changing other targets' latency stats.

**Done (2026-06-12).** `probe.targets` entries are now either bare strings
(weight 1) or `{relation: type#rel, weight: N}` maps (`TargetSpec` with custom
YAML unmarshal). Probe still samples every target equally; weights are
persisted in `corpus.json` (only when any differ from 1) and the load phase's
`corpusPicker` selects a target proportionally to weight, then an entry
uniformly within it. Without weights the picker is uniform over entries —
bit-for-bit the original behavior. Verified live: weight 8 on
`document#editor` among five targets produced a 66.3% request share
(expected 8/12 ≈ 66.7%).

### 12. Richer generation shapes: per-user-type fanout and value distributions ✅

**Motivation.** Two real frictions encountered while tuning for an actual
production model:
- Fanout applies per (object, relation, *each accepted user type*), so a
  relation accepting `[user, service, group#member]` with fanout 6 gets 18
  subjects. There's no way to say "6 group members, 0 direct services".
- Map/list condition parameters get a single fixed `keys:` count, but real
  datasets are skewed or bimodal (most role-style objects carry small maps, a
  few carry very large ones). Today that takes two bracketing runs.

**Sketch.**
- Extend fanout keys with an optional user-type suffix:
  `group#member@user: 8`, `group#member@service: 0`; the bare key remains the
  per-type default. Same for `wildcard_probability` as an optional per-relation
  map, keeping the global scalar as default.
- Extend `ParamGenConfig` with `keys_distribution: {values: [...], weights:
  [...]}` (or `min/max` + `skew`) so map sizes vary per tuple, drawn from the
  world's deterministic RNG.

**Files.** `config.go`, `seed.go`. Watch determinism: new draws must not
reorder existing RNG consumption for configs that don't use the new knobs
(gate the new code paths on the new keys being present).

**Accept.** A config can produce groups with user members but no service
members, and a tuple set whose condition map sizes follow a configured
bimodal distribution — verified by a unit test over generated tuples.

**Done (2026-06-12).** `seed.fanout` keys accept an optional `@usertype`
suffix (`document#editor@user: 0`, usersets as
`document#editor@group#member: 4`); the bare `type#relation` key stays the
default for unsuffixed user types. `seed.wildcard_probabilities` is a
per-relation map overriding the global `wildcard_probability` scalar. Both
are validated against the model: a suffix must name a user type the relation
directly accepts, and a wildcard-probability key must name a relation that
actually has a wildcard ref. `keys_distribution: {values: [...], weights:
[...]}` on a condition param draws map/list sizes per tuple (empty weights =
uniform; mutually exclusive with `keys`). Determinism is preserved by
construction: the per-type fanout lookup and per-relation probability lookup
replace constants without changing draw counts, and the distribution draw
only consumes RNG when configured — a unit test confirms no-op knobs produce
a byte-identical tuple graph. Unit tests cover the zero-direct-users /
userset-only shape, per-relation wildcard suppression, and a bimodal 1-or-8
map-size distribution; verified end-to-end with a shaped smoke config
(suffixed fanout + per-relation wildcard prob + bimodal keys) and the plain
example config, both with zero errors and zero mismatches.

### 13. Mismatch diagnostics ✅

**Motivation.** `verify_results` counts mismatches but discards *which* checks
mismatched, making the most alarming number in the report uninvestigable.

**Sketch.** Collect mismatched corpus entries (deduplicated, capped at e.g.
100) with observed-vs-expected, and write `results/mismatches-<stamp>.json`
when nonzero. Mention the file in the findings doc.

**Files.** `load.go`, `report.go`.

**Accept.** A run with `MINIMIZE_LATENCY` plus aggressive query caching and
background churn (item 10) produces an actionable mismatch file.

**Done (2026-06-12).** Workers record mismatched corpus entries
(observed-vs-expected) into a mutex-guarded recorder — locked only on
mismatches, so the hot path is untouched — deduplicated by check identity and
capped at 100. `Report.Save` writes `results/mismatches-<stamp>.json` when
nonempty, records the path in the results JSON, and the findings doc names
the file. Sweep runs merge records across steps. Verified mechanically by
flipping 30 corpus expectations and re-running: 1315 raw mismatches collapsed
to the 18 distinct flipped checks, all correctly attributed.

---

## P2 — Breadth and ergonomics

### 14. Additional endpoints: `list-objects`, `list-users`

`ListObjects` is OpenFGA's notoriously expensive query and a common production
pain point; a perf tool that can't measure it is incomplete. Reuse corpus
subjects/relations: for each entry, ask "which `<object type>` can `<user>`
`<relation>`?" Expected-result verification is harder (set-valued); start with
latency-only plus result-count distributions, and gate `verify_results` to
spot-checks against probe-known allowed pairs (each corpus entry's object
should appear in the listing when expected=true and consistency allows).
**Files:** `client.go`, `load.go`, `config.go` (`load.endpoint` enum),
`report.go`.

### 15. CLI flag overrides for common knobs ✅

The README currently recommends `sed` for a quick smoke run. Add flags that
override config after load: `-duration`, `-warmup`, `-rate`, `-concurrency`,
`-endpoint`, `-consistency`, `-output-dir`. Record overrides in the resolved
config snapshot (item 9). **Files:** `main.go`, `config.go`.

**Done (2026-06-13).** The shared flag block in `main.go` gained `-duration`,
`-warmup`, `-rate`, `-concurrency`, `-endpoint`, `-consistency`, and
`-output-dir`. Only flags actually passed (detected via `fs.Visit`) override
the loaded config; `Config.applyOverrides` applies them and re-runs `validate`,
so a bad override (`-consistency EVENTUAL`) fails fast. Because overrides mutate
`cfg` before `Resolved()` is marshaled, they appear in the results JSON's
`resolved_config` — a run stays reproducible from its output alone. The README's
`sed` smoke-test recipe is replaced with `-warmup 2s -duration 8s`. Verified
end-to-end: `all -warmup 2s -duration 5s -concurrency 8` reflected all three in
the load line and the resolved config. Unit test `TestApplyOverrides` covers
apply/no-op/bad-override.

### 16. Latency timeline in the report ✅

Per-second (or per-5s) p50/p99 + throughput series over the measured window,
as a table in JSON and a sparkline-ish markdown table. Catches cache fill-in,
GC pauses, and degradation that aggregate percentiles hide; cheap to compute
from existing samples if each sample records a completion timestamp.
**Files:** `load.go` (timestamp per sample), `report.go`.

**Done (2026-06-13).** `buildTimeline` (in `report.go`) buckets the measured
samples by completion time, anchored at the first measured sample. Bucket width
adapts (`timelineWidth`) so any run is ~12 rows: 1s for smoke runs, up to 1
minute for hour-long runs. Each bucket carries requests, throughput (items ÷
width), p50, p99, and errors; the series is `timeline` in the results JSON and a
"Latency over time" markdown section with a p99 sparkline bar scaled to the
worst bucket. For sweeps it reflects the knee step (built in `BuildReport`,
which `BuildSweepReport` calls on the headline result). No `load.go` change was
needed — samples already record `Completed` (P0 item 3). Verified live: a 10s
run surfaced the cache fill-in spike (t+0s p99 15.8ms vs steady-state ~8.5ms).
Unit tests `TestTimelineWidth` and `TestBuildTimeline` cover width selection,
bucketing, item-based throughput, and the empty case.

### 17. Optional raw sample export

`load.sample_file: samples.jsonl.gz` dumping per-sample target, latency,
outcome class, timestamp — for users who want their own analysis. Off by
default; write from the existing single collector goroutine to keep the hot
path clean. **Files:** `load.go`, `config.go`.

### 18. gRPC client option

Production callers typically use gRPC SDKs; HTTP-only measurement may
overstate latency. The bundled compose already exposes `:8081`. Keep the HTTP
client as default and the abstraction minimal: an interface over
`Check`/`BatchCheck` only, selected by `openfga.protocol: http|grpc`. This
pulls in protobuf deps — weigh against the "thin client" principle and keep it
strictly optional at build or config level. **Files:** `client.go`, new
`client_grpc.go`, `config.go`.

### 19. OIDC auth

Only pre-shared-key auth is supported. Managed/cloud FGA deployments use OIDC
client-credentials. Token fetch + refresh outside the hot path (background
refresh well before expiry so no request ever pays the token cost).
**Files:** `client.go`, `config.go`.

### 20. Seeding at scale

For datasets in the millions of tuples: progress/ETA output during seeding
(currently silent until done), resumable seeding (record high-water mark in
the state file), and a `fgaperf plan` subcommand that prints expected tuple
counts per relation from config alone — no server — so users can sanity-check
graph size before a long seed. Document (rather than build) the
seed-once-then-`pg_dump`/restore workflow for repeated large-scale runs.
**Files:** `seed.go`, `main.go`, README.

### 21. CI hardening

- Matrix the integration job across OpenFGA versions (pinned current +
  `latest`) to catch API drift early.
- Ensure the example model used in CI exercises every generator feature:
  conditions with map params, wildcard-with-condition, userset subjects,
  intersection, exclusion, and a contextual relation — so regressions in
  `seed.go`/`probe.go` fail CI rather than surfacing only against private
  models.
- Add a golden test for `Report.Markdown()` with a fixed `Report` struct.
- Run the race detector (`go test -race`) — the load path is concurrent.
**Files:** `.github/workflows/ci.yaml`, `examples/`, `report_test.go` (new).

### 22. Docs: a short benchmarking-methodology page

A `docs/methodology.md` covering: closed-loop vs fixed-rate (when each lies to
you), coordinated omission, warmup and cache-fill, corpus uniqueness vs query
caching, why `HIGHER_CONSISTENCY` probing + `MINIMIZE_LATENCY` load can
legitimately mismatch, client/server co-location, and "change one variable per
run". Link it from the findings doc's caveats section. **Files:** `docs/`,
`report.go`, README.

---

## P2 — User-friendliness and onboarding

These items lower the bar for users who aren't already fluent in OpenFGA or
performance testing. None are correctness fixes; all of them reduce
time-to-first-useful-run or time-to-understand-the-numbers. The README
glossary, annotated `examples/config.yaml`, `docs/configuration-reference.md`,
findings-doc inline explainers, footer legend, and expanded `inspect` legend
already landed (2026-06-13); the items below are the next layer.

### 23. Live progress during probe and load phases

**Motivation.** After the header line, `probe` and `run` are silent for
seconds to minutes. A first-time user can't tell the difference between
"working" and "hung", and an experienced user can't tell early whether the
run will need to be killed and reconfigured. Item 20 covers the same idea for
`setup`; this item covers probe and load.

**Sketch.**
- `probe`: print `probed N/M targets, current target: T (allowed/denied so
  far: A/D)` every ~2s from a background goroutine. Compute remaining ETA
  from rolling probe latency.
- `run`: print `t+12s of 60s | 4892 req/s | p99 7.8ms | 0 errors` every
  ~5s. During warmup, label the line `warmup` and zero the percentiles so a
  user knows they're not the headline number.
- Detect non-TTY (`!isatty(stderr)`) and skip the live output, so CI logs
  stay clean.

**Files.** `probe.go`, `load.go`, possibly new `progress.go` for the rolling
window.

**Accept.** A `fgaperf all` run with no terminal output flag continuously
prints progress lines; a `fgaperf all 2>/dev/null | cat` run produces only
the existing summary lines.

### 24. `fgaperf doctor` / pre-flight checks

**Motivation.** New users hit the same handful of failure modes before they
ever get to a measurement: OpenFGA not running, wrong port, no permissions to
create stores, metrics endpoint not exposed, datastore is `memory` (so
numbers are useless), model JSON doesn't match the running OpenFGA version.
Diagnose them once, up front.

**Sketch.** `fgaperf doctor -config config.yaml` runs a checklist and prints
a pass/fail line per check with an actionable hint on each fail:
- HTTP reachability to `openfga.api_url`.
- `CreateStore` + `DeleteStore` round-trip with a temp name.
- Model file parses; `WriteModel` to the temp store succeeds.
- `metrics.prometheus_url` reachable (when set), with required metric
  families present (`openfga_request_duration_ms`,
  `openfga_datastore_query_count`).
- Server reports a non-`memory` datastore if we can determine it from
  metrics labels (warn, not fail, since it's only detectable indirectly).
- A pre-flight short-form lives inside `setup`/`run`/`all` too: on
  connection refused, print "OpenFGA not reachable at <url> — try `docker
  compose ps`" instead of a bare error.

**Files.** New `doctor.go`, `main.go`, `client.go` (a few capability probes).

**Accept.** With the compose stack stopped, `fgaperf doctor` prints a clear
"OpenFGA not reachable at http://localhost:8080 — run `docker compose up
-d`" message and exits non-zero; with the stack running, every check
passes.

### 25. Actionable error wrapping

**Motivation.** Today's errors are technically accurate but
unhelpful-on-the-first-read. "store not found" doesn't tell a user the state
file is stale; a YAML `unknown field` error doesn't suggest the nearest known
key; a `connection refused` from the load phase doesn't suggest port
checking. Each unfriendly error is a stall.

**Sketch.**
- YAML `KnownFields` rejection: catch and re-emit with line/column (already
  available from `yaml.v3`'s position info) plus a Levenshtein-nearest
  known key from the schema (e.g. "did you mean `allowed_ratio`?").
- "store not found" / 404 on the recorded store ID: suggest "ran
  `fgaperf cleanup` to clear stale `.fgaperf-state.json`?".
- Connection refused / dial timeout: include `openfga.api_url` and
  suggest `docker compose ps` if the URL is localhost.
- Unauthorized (401/403): suggest `openfga.api_token` and the
  `OPENFGA_AUTHN_METHOD` server flag.
- Model file missing or unparseable: print the exact path searched and the
  CLI command to produce it (`fga model transform`).

**Files.** `main.go`, `client.go`, `config.go`. Consider a small
`errors.go` for the suggestion helpers.

**Accept.** Each of the five error classes above produces a message naming
the suspected cause and the command to verify or fix it.

### 26. `fgaperf plan` — server-free dry run

**Motivation.** Users iterate on their config blind: they tweak fanout,
cohorts, or probe targets and have to spin up a full run (or do mental math
across `seed.go`) to know what they actually configured. A no-server preview
collapses the loop. Distinct from item 20's seeding-at-scale focus — this is
for configuration iteration, not large-deployment planning.

**Sketch.** `fgaperf plan -config config.yaml` loads the model + config,
applies defaults, runs `validate` and `validateAgainstModel`, then prints:
- The resolved config (post-defaults YAML, secrets redacted) — same payload
  `compare` uses.
- Per-type instance counts and totals.
- Per-relation expected tuple counts (from fanout × instance counts ×
  accepted user types), with totals.
- Probe sample budget: targets × `samples_per_target`, plus expected
  corpus size after resampling.
- A duration estimate for the load phase only (warmup + duration).
- Warnings: probe targets that would produce ~0 corpus entries (no
  assignable refs reachable), conditions referenced without a `pools`
  entry, etc.

Add `fgaperf validate -config X` as an alias that runs only the
validation/resolved-config print, no estimates.

**Files.** New `plan.go`, `main.go`, `seed.go` (expose expected-count
calculators without generating tuples).

**Accept.** Running `fgaperf plan` on `examples/config.yaml` exits 0 with a
report whose tuple-count totals match what an actual `setup` would seed;
running it on a config whose `probe.targets` includes a relation with no
assignable subjects prints a warning naming the target.

### 27. Findings TL;DR headline line

**Motivation.** The findings doc is dense. A reader who just wants the
upshot — to paste into a Slack message or compare to last week's run —
should not have to scroll. Sweep runs already get a knee-rate sentence;
fixed-rate and closed-loop runs deserve the same.

**Sketch.** A single-paragraph "Summary" section between the test
configuration table and headline results, with three to five facts:
sustained throughput, p99, CEL-vs-unconditioned delta, mismatch count,
notable callout (saturation, errors, write churn). Sweep runs get a knee
line in the same slot. Keep it generated, not hand-written — same template
filled with the headline numbers already in `Report`.

**Files.** `report.go`.

**Accept.** Every findings doc starts with a one-paragraph summary; the
example findings doc gains a summary that names throughput, p99, and the
zero-mismatch result.

### 28. "What you might change" hints in findings

**Motivation.** The findings doc reports what happened; it doesn't suggest
what to do about it. The most common failure of a first run isn't the
server's fault — it's a config that produced an unrepresentative corpus or
the wrong probe target mix. A heuristic suggestion section turns the report
into a guide for the next run.

**Sketch.** A new `## Suggestions` section, only rendered when at least one
heuristic fires. Conservative phrasing ("consider", not "do"). Initial
rules:
- Corpus duplication > 2x on any target → suggest raising
  `probe.samples_per_target` or setting `allowed_ratio: -1` for that
  target's natural mix.
- Achieved < 98% of offered on a non-sweep fixed-rate run → suggest a
  sweep across rates below the offered one.
- Mismatches > 0 with `MINIMIZE_LATENCY` and `write_rate > 0` → call it
  expected (cache invalidation lag) and point at `HIGHER_CONSISTENCY` if
  fresh reads matter.
- Server-side p99 ≪ client p99 → suggest checking client/server
  co-location.
- `seed.cohort_bias` low (< 0.5) on a model with intersections →
  suggest raising it (corpus likely all-denied).
- Closed-loop run on a model with `load.write_rate: 0` → mention sweep
  + write_rate as the more realistic measurement.

Each rule is one function `(r *Report, cfg *Config) (string, bool)`; the
section iterates the registered rules. Easy to extend later.

**Files.** `report.go`, possibly a new `suggestions.go`.

**Accept.** Re-running the example config produces zero suggestions
(healthy run); a deliberately broken config (cohort_bias 0.1, intersection
target) produces a Suggestions section naming cohort_bias.

### 29. ANSI color and bold on isatty stdout/stderr

**Motivation.** Pure polish, but disproportionate. Headline numbers in
bold, errors in red, warnings in yellow, and the most common new-user
failure (compose stack down) gets a tinted help block. Turns "this feels
like a hobby tool" into "this feels supported".

**Sketch.** A tiny color helper (`color.Bold(s)`, `color.Red(s)`,
`color.Yellow(s)`, `color.Dim(s)`) that no-ops when
`!isatty(fd)` or `NO_COLOR` is set (https://no-color.org). Apply to:
- Per-phase summary lines (`throughput:`, `server:`, etc.).
- Doctor checklist (✓/✗ in green/red).
- The probe duplication warning (yellow).
- `fail()` output (red).
- Findings markdown stays plain — pipes and editors render that.

**Files.** New `color.go`, `main.go`, `probe.go`, `load.go`.

**Accept.** `fgaperf all | cat` produces output with no ANSI escapes;
running it directly in a terminal shows bold headline numbers and a
red error if the server is unreachable.

### 30. `docs/getting-started.md` — narrative walkthrough

**Motivation.** The README is reference-shaped. A reader who is new to
OpenFGA, has a model, and just wants to know "what do I do next" needs a
linear walkthrough. Keeps the README focused on reference; the new doc
covers the path through the tool.

**Sketch.** ~600 words, in order:
1. What fgaperf measures (one paragraph; link to README glossary).
2. Bring up the server (compose link).
3. `fgaperf inspect` against the example model; explain the output.
4. First smoke run (`all` with shortened warmup/duration).
5. Reading the findings doc: walk the example findings end-to-end with
   "this number means…".
6. One tuning loop: bump `seed.cohorts`, rerun, `fgaperf compare`.
7. Where to go next (configuration reference, methodology page).

Link from the README's Quick Start.

**Files.** New `docs/getting-started.md`, README.

**Accept.** A reader following the doc top-to-bottom against a fresh
clone reaches a successful smoke run and a comparison without consulting
the README reference once.

### 31. `docs/recipes.md` — short configuration recipes

**Motivation.** Most users come in with a specific question, not a desire
to learn the whole tool. A recipe page indexes the tool by question.

**Sketch.** One recipe per common scenario, each 5–10 lines of YAML plus
one explanatory paragraph:
- Find this server's max sustained throughput.
- Measure cache impact (`MINIMIZE_LATENCY` vs `HIGHER_CONSISTENCY`,
  same store).
- Compare two model versions (same load, two model files).
- Test under realistic write churn.
- Spot a hot relation (probe targets weighted to one).
- Reproduce a noisy run exactly (`random_seed`).
- Run before/after a server upgrade.

Cross-link to relevant configuration-reference sections.

**Files.** New `docs/recipes.md`, README.

**Accept.** Each recipe runs as-is against the bundled compose stack and
produces a non-trivial findings doc (zero mismatches, populated tables).

### 32. Per-subcommand `--help` with examples

**Motivation.** `fgaperf` with no args prints a one-line usage. Each
subcommand should print its purpose, flags, one worked example, and the
most common gotcha — at the moment the user is one keystroke from
needing it.

**Sketch.** Replace the single `flag.NewFlagSet` per subcommand with a
small map of `(name, summary, longHelp, flags, example)`. `fgaperf
<cmd> -h` prints the long form; `fgaperf -h` lists subcommand summaries
and points at `<cmd> -h` for each.

Examples worth writing into help text:
- `setup -h`: "writes `.fgaperf-state.json`; needs OpenFGA reachable;
  re-running creates a fresh store".
- `probe -h`: "needs `setup` to have run; reads model + state, writes
  `corpus.json`".
- `run -h`: gotcha — "needs both setup and probe; sweep and rate are
  mutually exclusive".
- `cleanup -h`: gotcha — "`-all-stores` deletes by name, not by ID; use
  when state file is gone".

**Files.** `main.go`.

**Accept.** Every subcommand responds to `-h` with the long-form help,
including a worked example; `fgaperf -h` lists every subcommand with its
one-line summary.

### 33. README status badges

**Motivation.** Quick visual signal that the project is alive and
configured. Costs nothing to add.

**Sketch.** Add badges to the README header for: CI status (GitHub
Actions), license (Apache-2.0), Go version (from `go.mod`). Shield.io
URLs only; no external trackers.

**Files.** `README.md`.

**Accept.** README renders with three badges above the intro paragraph;
the CI badge reflects `main`'s actual state.

### 34. `docs/troubleshooting.md` — common failure modes

**Motivation.** Even with item 25 (actionable errors), users will hit
multi-step problems that need a paragraph, not a one-line hint. Five
problem/fix entries cover most first-week issues.

**Sketch.** Five entries, problem → diagnosis → fix:
1. "OpenFGA not reachable" (port mismatch, docker not started,
   colima/docker context).
2. "Store not found" mid-run (state file stale, `cleanup`).
3. "Probe corpus is empty / all-denied" (cohort_bias, target with no
   assignable refs, model mismatch).
4. "Verification mismatches under churn" (cache + consistency tradeoff;
   when it's expected vs a real bug).
5. "Numbers don't match production" (datastore type, client placement,
   warmup too short).

Each links to the relevant configuration-reference section.

**Files.** New `docs/troubleshooting.md`, README.

**Accept.** Each of the five scenarios has a complete walkthrough; the
README links to the doc from a new "If something goes wrong" line near
Quick Start.

### 35. `fgaperf inspect --json`

**Motivation.** Today `inspect` prints a terminal-formatted summary;
users with their own tooling have to re-parse the model JSON from
scratch to get the same information (assignable relations, CEL
reachability, condition param split). Emitting the analysis as JSON
makes the tool composable.

**Sketch.** A `--json` flag on `inspect` that prints the `Analysis`
struct (or a serialization-friendly projection of it: types, relations
with `[assignable, CEL, contextual]` tags, conditions with param
splits) as JSON to stdout and skips the human-readable summary. Stable
schema (this is now an API surface for downstream tools).

**Files.** `main.go`, `model.go` (a `MarshalJSON` projection on
`Analysis` if the natural shape isn't right).

**Accept.** `fgaperf inspect --json -config examples/config.yaml | jq
'.relations[] | select(.tags | contains(["CEL"])) | .key'` lists the
CEL-reachable relations.

---

## Suggested sequencing

1. **Trust the numbers:** items 1–5 (small, mostly independent diffs).
2. **See the server side:** item 6, then 9 (both feed everything later).
3. **Answer the capacity question:** items 7 and 8 together — sweep produces
   the data, compare makes it consumable.
4. **Realism:** items 10–13 as needed by actual investigations.
5. **Breadth:** P2 items opportunistically; item 14 (`list-objects`) first if
   any consumer of the tool uses that API in production.
6. **User-friendliness:** items 23, 25, 24 first (live progress, actionable
   errors, doctor) — they remove the most common first-run stalls. Then
   27 + 28 (findings TL;DR and suggestions) to make output self-explanatory.
   Then 30 + 31 + 34 (docs walkthrough, recipes, troubleshooting) to round
   out onboarding. 26 (`plan`) and 32 (per-subcommand help) are independent
   and small enough to slot in anywhere. 29 (color) and 33 (badges) are
   polish; 35 (`inspect --json`) only when a downstream tool needs it.

## Invariants to re-verify after any change

- `go vet ./... && go test ./...` clean.
- `./fgaperf all` with the example config against a fresh local OpenFGA
  completes, deletes its store, and writes both result files.
- Same `random_seed` + same config ⇒ identical generated tuples and corpus
  candidates (determinism is part of the contract; add a regression test that
  hashes generated tuples for a fixed config if generation code is touched).
- No type, relation, right, or tenant names from any private model appear in
  code, examples, docs, or this file. Private inputs live only in gitignored
  paths (`main-model.json`, `config.yaml`, `models/`, `tests/`).
