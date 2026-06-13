# fgaperf Bug Report

Audit date: 2026-06-13

This report is based on a source, test, and documentation scan of the public
repository files. Private/gitignored inputs such as `config.yaml`,
`main-model.json`, `models/`, and `tests/` were not inspected.

## Fix Progress

Updated 2026-06-13:

- Fixed #1 by surfacing `batch-check` item errors and missing correlation IDs
  as measured sample errors (`batch-item`) while still verifying successful
  items. Added regression coverage in `TestBatchCheckItemErrorsAreReported`.
- Fixed #2 by recording/counting mismatches only after the warmup boundary.
  Added `TestWarmupMismatchesExcludedFromMeasuredCounts`.
- Fixed #3 by tracking YAML field presence before applying defaults, so
  explicit zero values for wildcard probability, probe mix knobs, duplication,
  and warmup survive. Added `TestExplicitZeroConfigValuesSurviveDefaults`.
- Fixed #4 by validating negative and invalid numeric knobs across seed,
  pools, conditions, OpenFGA timeout, load timing, and sweep timing.
- Fixed #5 by making corpus distinctness and mismatch deduplication use a
  canonical request identity that includes context and contextual tuples.
- Fixed #6 by validating condition parameter names against the model and
  honoring explicit `tuple_params: []`.
- Fixed #7 by failing `run` when state and corpus store/model IDs disagree.
- Fixed #8 by generating timestamp condition values from the seeded RNG rather
  than wall-clock time.
- Fixed #9 by opening `sample_file` before starting load workers and returning
  raw sample encode/close errors.
- Fixed #10 by labeling `batch-check` findings as a batch breakdown instead of
  a per-relation breakdown.
- Fixed #11 by URL-escaping `cleanup -all-stores` continuation tokens.
- Fixed #12 by adding compare caveats for offered rate, warmup, write churn,
  and sweep-vs-single-run shape.
- Fixed #13 by documenting the shared-traffic caveat for Prometheus metrics in
  generated findings, README, config reference, methodology, example config,
  and example findings.
- Fixed #14 by listing all supported endpoints in CLI help.
- Fixed #15 by validating resume high-water marks before slicing generated
  tuples.

Verification so far:

- `GOCACHE=/tmp/fgaperf-go-build go test ./...`
- `GOCACHE=/tmp/fgaperf-go-build go vet ./...`
- `GOCACHE=/tmp/fgaperf-go-build go build -o /tmp/fgaperf-audit-bin .`
- `GOCACHE=/tmp/fgaperf-go-build go test -race ./...`

## Verification Run

The basic toolchain checks pass:

- `GOCACHE=/tmp/fgaperf-go-build go test ./...`
- `GOCACHE=/tmp/fgaperf-go-build go vet ./...`
- `GOCACHE=/tmp/fgaperf-go-build go build -o /tmp/fgaperf-audit-bin .`
- `GOCACHE=/tmp/fgaperf-go-build go test -race ./...`

Notes:

- The build needed elevated filesystem access because Go tried to write its
  module stat cache under the user module cache.
- The race-enabled tests needed elevated access because `httptest` binds
  loopback listeners.

## High Severity

### 1. BatchCheck item errors are reported as successful requests

Source:

- `client.go:343-349` models per-item `BatchCheck` errors.
- `load.go:617-623` only checks mismatches for result items whose `Error` is
  nil.

Problem:

`BatchCheck` can return HTTP 200 while individual correlation IDs contain an
`error`. `doBatch` treats that whole request as successful unless the outer
HTTP call fails. Item-level errors are not counted in `ErrorsByClass`, are not
included in error samples, and are ignored for verification. Missing
correlation IDs are also ignored.

Impact:

The report can show zero errors and clean verification while some checks in
each batch actually failed or were omitted. This is especially risky because
`batch-check` is meant to raise per-request throughput, so one bad response can
hide many failed logical checks.

Suggested fix:

Iterate over every requested correlation ID after a batch response. Treat a
missing ID or a non-nil item error as an error sample, or add explicit
item-error accounting distinct from request-level HTTP errors. Continue
verifying successful items.

### 2. Warmup mismatches leak into measured-phase reports

Source:

- `load.go:446-454` increments mismatch counters before checking whether the
  sample completed after warmup.
- `load.go:523-526` persists those all-phase mismatch counts and records.

Problem:

Warmup samples are excluded from `res.Samples`, latency statistics, error
classes, and raw sample export, but result mismatches are counted before the
warmup boundary check. The final report says the mismatch count belongs to the
measured run, while it may include warmup-only mismatches.

Impact:

A transient warmup mismatch can produce a nonzero `result_mismatches` count and
write a mismatch file even when every measured-phase response matched the probe
expectation.

Suggested fix:

Only increment `mismatches` and record mismatch details inside the
`s.Completed.After(warmupEnd)` branch, or report warmup mismatches separately.

### 3. Valid zero values are silently replaced by defaults

Source:

- `config.go:485-498` defaults zero-valued wildcard/probe knobs.
- `config.go:502-506` defaults zero warmup/duration.

Problem:

Several fields use zero as both "not configured" and a meaningful user value:

- `seed.wildcard_probability: 0` should disable wildcard tuple generation but
  becomes `1.0`.
- `probe.cohort_bias: 0` should force cross-cohort/random subject selection but
  becomes `0.85`.
- `probe.allowed_ratio: 0` should create an all-denied target mix but becomes
  `0.5`.
- `probe.max_duplication: 0` is documented in code as unbounded but becomes
  `5`.
- `load.warmup: 0s` in YAML should mean no warmup but becomes `10s`.

CLI overrides can set some zeros because they are applied after defaults, but
YAML cannot express these values.

Impact:

Users can run a benchmark believing they disabled a behavior or selected a
boundary case, while the resolved config uses a different value. This directly
changes the tuple graph, corpus mix, and measured load window.

Suggested fix:

Use pointer fields or a custom defaults layer that can distinguish absent
fields from explicit zero values. Add tests for intentional zero values and
verify `Resolved()` preserves them.

### 4. Numeric validation gaps can cause panics or silent graph changes

Source:

- `config.go:314-352` validates only a subset of numeric fields.
- `seed.go:45-53` allocates per-type instance slices from config values.
- `config.go:590-603` allocates generated pool values from `PoolConfig.Count`.
- `seed.go:120-153` uses fanout values directly in generation loops.

Problem:

Several numeric inputs are not validated:

- Negative `seed.instances.<type>` values can panic with `make([]string, n)`.
- Negative `pools.<name>.count` can panic with `make([]string, n)`.
- Negative `seed.default_fanout` or `seed.fanout` values silently produce no
  tuples for affected relations.
- Negative `conditions.*.params.*.keys` values are silently ignored.
- `openfga.timeout`, `load.warmup`, `load.duration`, and
  `load.sweep.step_duration` are not rejected when negative.

Impact:

Misconfiguration can crash `setup`/`probe`, create an unexpectedly sparse graph,
or produce empty/invalid load runs without a clear configuration error.

Suggested fix:

Validate all non-negative and positive numeric fields explicitly. In
particular, validate per-type instance counts, all fanout values, pool counts,
condition key counts, OpenFGA timeout, warmup, duration, and sweep step
duration.

## Medium Severity

### 5. Corpus uniqueness ignores request context and contextual tuples

Source:

- `probe.go:20-29` stores `ContextualTuples` and `Context` on each entry.
- `probe.go:91` defines uniqueness as only `user|relation|object`.
- `probe.go:95-117` uses that key for corpus distinct counts.
- `load.go:159-170` uses that key for mismatch deduplication.

Problem:

Two entries with the same `(user, relation, object)` but different request
context or contextual tuples are counted as the same distinct check. The same
triple can legitimately have different expected outcomes when CEL context or
request-scoped tuples differ.

Impact:

`corpus_distinct` and target duplication warnings can undercount effective
request diversity. Mismatch records can also deduplicate away useful evidence
when the same triple fails only for a particular context.

Suggested fix:

Build a stable request identity that includes the tuple key plus canonicalized
`Context` and `ContextualTuples` JSON. Use it consistently for distinct stats
and mismatch deduplication.

### 6. Condition parameter overrides are under-validated

Source:

- `config.go:430-433` validates condition names but not parameter names.
- `model.go:257-264` only treats `tuple_params` as an override when the list is
  non-empty.
- `seed.go:309-318` ignores unknown `conditions.*.params.*` keys.

Problem:

The config can name nonexistent condition parameters in `tuple_params` or
`params` without failing validation. Also, an explicit empty `tuple_params: []`
cannot force all parameters onto the request side because the override branch
requires `len(tuple_params) > 0`.

Impact:

Typos in condition shaping silently change the CEL data sent to OpenFGA.
Operators cannot express "no tuple-side params" for a condition whose default
heuristic would put map/list params on the tuple.

Suggested fix:

During model validation, check every `tuple_params` entry and every
`params` key against the condition's declared parameters. Represent
`tuple_params` as a pointer slice or track YAML field presence so an explicit
empty list is different from omission.

### 7. `run` can combine a corpus with unrelated state metadata

Source:

- `main.go:369-398` loads the corpus for requests but uses the state for
  tuple-count and seed-duration report metadata.
- `load.go:564-569`, `load.go:604-608`, `load.go:636-644`, and
  `load.go:679-687` send requests using `corpus.StoreID` and `corpus.ModelID`.

Problem:

The load phase gets the store/model IDs from `corpus.json`, while the report
gets tuple count and seed duration from `.fgaperf-state.json`. There is no
check that the state file and corpus describe the same store/model.

Impact:

After switching configs, restoring old artifacts, or running separate phases
out of order, a run can benchmark one store while reporting tuple metadata from
another. Conversely, `run` requires a state file even though the corpus already
contains the IDs needed to replay traffic.

Suggested fix:

Persist tuple count and seed duration in the corpus, or validate
`State.StoreID`/`ModelID` against `Corpus.StoreID`/`ModelID` before running.
Fail fast on mismatch.

### 8. Timestamp condition generation is not reproducible

Source:

- `seed.go:340-341` returns `time.Now().UTC()` for
  `TYPE_NAME_TIMESTAMP`.

Problem:

Most generated values are derived from `random_seed`, but timestamp parameters
use wall-clock time. This can affect tuple-side condition context when a user
overrides timestamp params into `tuple_params`, and request-side context during
probe corpus construction.

Impact:

The same model, config, and `random_seed` can produce different tuple contexts
or corpus entries across runs, weakening the reproducibility guarantee.

Suggested fix:

Generate timestamps from a deterministic base time plus seeded offsets, or add
a config field for a fixed clock base.

### 9. `sample_file` handling starts workers before file setup succeeds

Source:

- `load.go:459-476` starts workers, then opens `sample_file`.
- `load.go:493-501` writes samples and only reports close errors.
- `load.go:274-292` ignores `json.Encoder.Encode` errors.

Problem:

If `sample_file` cannot be opened, `RunLoad` returns an error after workers and
the fixed-rate scheduler have already been started. In CLI use the process soon
exits, but as a library/test path this can leak goroutines or continue sending
traffic. Once the writer is open, encode errors are ignored, so disk/full or IO
errors can silently drop raw sample records until close, and plain-file write
errors may never be surfaced.

Impact:

Bad sample export configuration can perturb or partially run a load test before
the user sees the error. Raw sample exports may be incomplete without a clear
failure.

Suggested fix:

Open and validate the sample writer before launching workers. Make
`sampleWriter.write` return an error and cancel/stop the run on persistent
write failure.

### 10. Batch-check loses per-relation breakdowns

Source:

- `load.go:610` records every batch sample with `Target: "batch"`.
- `report.go:567-584` labels the following table as per-relation breakdown.

Problem:

A batch can contain entries from several target relations, but the sample is
reported under a single synthetic `batch` target.

Impact:

For `load.endpoint: batch-check`, the generated findings lose the main
per-relation diagnostic that the rest of the tool emphasizes. A slow relation
inside a mixed batch cannot be identified from the report.

Suggested fix:

Either collect per-item latency/error attribution where possible, or report
batch composition by target so the user can see which relations contributed to
each batch population. At minimum, rename the table for batch-check so it does
not promise a per-relation view.

## Low Severity / Shortcomings

### 11. `cleanup -all-stores` does not escape continuation tokens

Source:

- `client.go:241-244`

Problem:

`ListStores` appends `continuation_token` directly into the query string.
Continuation tokens often contain characters such as `+`, `/`, `=`, or `&`
that must be URL-escaped.

Impact:

Store cleanup can fail or page incorrectly once a token contains reserved query
characters.

Suggested fix:

Build the query with `url.Values` or at least `url.QueryEscape(token)`.

### 12. Compare caveats miss major load-shape differences

Source:

- `compare.go:30-49`

Problem:

`comparabilityCaveats` warns on endpoint, corpus size, duration, concurrency,
and tool version, but not on offered rate, warmup, background write churn, sweep
mode, or consistency mode.

Impact:

Two fixed-rate runs at very different offered rates can be presented without a
warning even though their latency and saturation behavior are not directly
comparable. Write churn differences have a similar effect.

Suggested fix:

Add caveats for `OfferedRate`, `Warmup`, `WriteRate`, sweep-vs-single-run
shape, and possibly consistency. Consistency may be an intentional comparison,
so wording can say "differs" rather than "invalid".

### 13. Server-side metrics can be polluted by unrelated traffic

Source:

- `metrics.go:22-25` intentionally aggregates histograms across label sets.
- `metrics.go:258-274` builds a diff from the two global snapshots.

Problem:

The Prometheus diff assumes only this load run is touching the OpenFGA server
during the measured window. It aggregates all matching metric families across
label sets and does not isolate store/model/client labels.

Impact:

On a shared OpenFGA deployment, unrelated requests can skew server-side p99,
datastore-query counts, dispatch counts, and cache hit rates. The client-side
numbers remain scoped to fgaperf, but the server-side view may not be.

Suggested fix:

Document this caveat prominently near `metrics.prometheus_url`, and filter by
stable labels if OpenFGA exposes suitable store/model/method labels.

### 14. CLI endpoint help omits list endpoints

Source:

- `main.go:67`

Problem:

The `-endpoint` flag help says `check|batch-check`, but validation and docs
support `list-objects` and `list-users`.

Impact:

Users relying on `-h` may not discover the list endpoints.

Suggested fix:

Update the help text to `check|batch-check|list-objects|list-users`.

### 15. Resume trusts the saved high-water mark bounds

Source:

- `main.go:281-287` loads `SeededTuples` from state.
- `seed.go:403-405` slices `tuples[startIndex:]`.

Problem:

Resume checks that the total tuple count matches, but it does not check that
`SeededTuples` is between `0` and `TupleCount`.

Impact:

A corrupted or hand-edited state file can panic during resume instead of
returning a clear error.

Suggested fix:

Validate `0 <= SeededTuples <= TupleCount` before calling `SeedStore`.

## Suggested Fix Order

1. Fix batch-check item error accounting.
2. Move mismatch counting inside the measured-phase branch.
3. Rework config defaulting so explicit zero values survive.
4. Add missing validation for numeric fields and condition parameter names.
5. Expand corpus identity to include request context and contextual tuples.
6. Validate state/corpus consistency before `run`.
