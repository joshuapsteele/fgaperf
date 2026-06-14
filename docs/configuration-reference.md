# Configuration reference

Every fgaperf configuration field, what it does, the values it accepts, and
when you'd want to change it. Term definitions live in the README
[Glossary](../README.md#glossary); this document focuses on the knobs.

The annotated [examples/config.yaml](../examples/config.yaml) is the recommended
starting point — copy it, point `model_file` at your model, and tune from
there. Every field has a default; the minimum viable config is a `model_file`
path and an `openfga.api_url`.

For your own model, `fgaperf gen-config -model your-model.json > config.yaml`
emits an annotated starter with instance counts, fanout overrides, contextual
guesses, conditions, and pools picked from the model's shape. Treat the
output as a starting point rather than a final answer — the size and traffic
mix knobs still need tuning for your workload.

Configs are validated strictly. Unknown keys, out-of-range numbers, and
references to model objects that don't exist all fail fast with an error
naming the bad key.

---

## Top-level

| Field | Default | Description |
|---|---|---|
| `model_file` | `model.json` | Path to a compiled OpenFGA model JSON. Export from a `.fga` file with `fga model transform --file model.fga > model.json`. |
| `state_file` | `.fgaperf-state.json` | Where `setup` records the store ID + tuple count for later phases to read. Delete this file to force a fresh setup. |
| `corpus_file` | `corpus.json` | Where `probe` writes its corpus and `run` reads it from. |
| `corpus_source` | `probe` | How `probe` builds the corpus. `probe` synthesizes candidates from the model (the default path). `replay` instead reads a real check log from `replay.file` (see the [`replay`](#replay--corpus-from-a-real-check-log) section). |
| `output_dir` | `results` | Where `results-<stamp>.json`, `findings-<stamp>.md`, and (if any) `mismatches-<stamp>.json` are written. |
| `random_seed` | `0` (time-based) | Fixed seed makes generation, probing, and request ordering reproducible. Same seed + same config = same run. |
| `keep_store` | `false` | When `true`, `fgaperf all` does not delete the store at the end. Useful when iterating on probe/run without re-seeding. |

## `openfga` — server connection

| Field | Default | Description |
|---|---|---|
| `openfga.api_url` | `http://localhost:8080` | OpenFGA HTTP API base URL. Used for setup, probe, and (when `load.transport: http`) the measured phase. |
| `openfga.grpc_url` | derived | gRPC dial target (`host:port`) for `load.transport: grpc`. Unset derives `host:8081` from `api_url` (OpenFGA's default gRPC port), so the bundled compose stack and the common localhost case need no extra config. |
| `openfga.grpc_tls` | `false` | Dial the gRPC endpoint over TLS (system root CAs). Leave `false` for the local compose stack; set `true` for managed/cloud OpenFGA. Only used when `load.transport: grpc`. |
| `openfga.store_name` | `fgaperf` | Name used when creating the store. `cleanup -all-stores` matches on this. |
| `openfga.api_token` | unset | Pre-shared API token when `OPENFGA_AUTHN_METHOD=preshared`. |
| `openfga.oidc` | unset | OIDC client-credentials auth for managed/cloud OpenFGA (mutually exclusive with `api_token`). Sub-keys: `token_url`, `client_id`, `client_secret` (required), `audience`, `scopes` (optional). The token is fetched and refreshed in the background, off the request hot path; `client_secret` is redacted in the results snapshot. |
| `openfga.timeout` | `10s` | Per-request HTTP timeout. Raise if your real production timeout is higher, or to stop spurious timeouts dominating an under-provisioned datastore run. |

## `seed` — tuple graph generation

These knobs decide how big and dense the simulated authorization graph is.
The hardest one to think about is **fanout** — and getting it right is what
makes the difference between "every check denies because nothing connects"
and "every check allows because the graph is saturated".

| Field | Default | Description |
|---|---|---|
| `seed.cohorts` | `5` | Tenant-like partitions. Tuples are biased to link within the same cohort so intersections and tuple-to-userset relations resolve to "allowed" at realistic rates. Set roughly to the number of tenants you want to simulate. `1` means a single global tenant. |
| `seed.default_instances` | `25` | Instance count for any type not listed in `instances`. |
| `seed.instances` | empty | Per-type instance counts, e.g. `user: 1000`. Names must exist in the model — typos fail fast. |
| `seed.default_fanout` | `2` | Default tuples per `(object, relation, allowed user type)`. |
| `seed.fanout` | empty | Per-relation fanout overrides, keyed `type#relation` or `type#relation@usertype`. The `@usertype` suffix targets one accepted user type (the bare key still applies to the others). Usersets keep their `#`: `document#viewer@group#member`. |
| `seed.batch_size` | `100` | Tuples per Write API call. `100` matches OpenFGA's default server cap. |
| `seed.writers` | `8` | Concurrent Write workers during seeding. |
| `seed.wildcard_probability` | `1.0` | Probability each object gets a wildcard (e.g. `user:*`) tuple where the model allows one. The example config sets `0.5` to leave half of documents non-public. |
| `seed.wildcard_probabilities` | empty | Per-relation overrides of the scalar above, keyed `type#relation`. |

### Fanout sizing

- **Direct relations**: fanout = "how many users hold this directly". For a
  group's `member` relation, that's average group size. For a document's
  `editor`, that's average number of editors. Typical apps want `member`
  large (8–50) and document-level relations small (1–4).
- **Parent / link relations** (`folder#parent`, `document#parent`): use `1`
  unless you genuinely have multi-parent objects. Higher values multiply
  every downstream check by another tree to traverse.
- **Recursive relations** (`folder#parent` pointing into another folder):
  fgaperf only ever links higher-indexed instances to lower-indexed ones,
  so the graph stays a DAG no matter what fanout you set. But deep
  recursion still costs latency under load; if you want a flat hierarchy,
  reduce instance counts at the recursive type.

### Wildcards (public access)

A wildcard tuple grants a relation to "everyone" of a user type — e.g.
`user:* is viewer of document:42` makes the document publicly viewable. With
CEL conditions, this becomes "public *if the request matches a condition*",
which is a common pattern for scope/role gates. `wildcard_probability` is the
chance each eligible object gets such a tuple.

## `contextual` — request-scoped tuples

| Field | Default | Description |
|---|---|---|
| `contextual.relations` | empty | Direct relations that should be sent as [contextual tuples](https://openfga.dev/docs/interacting/contextual-tuples) on each Check rather than persisted during setup. Keyed `type#relation`. |
| `contextual.attach_probability` | `1.0` when relations set, else unused | Fraction of sampled checks that carry their contextual tuples. Use `1.0` if every production caller sends context; use less than `1.0` to include denied missing-context paths in the corpus. |

Use a contextual relation when the fact in question is request-local: "the
caller's session is active right now", "the device is currently in this
country", "the OAuth token includes scope X". Persisting these as regular
tuples would be wrong (they expire with the request) and slow (constant
write churn).

## `probe` — corpus construction

| Field | Default | Description |
|---|---|---|
| `probe.targets` | all relations | Relations to measure. Each entry is either a bare `type#relation` string (weight 1) or `{relation: type#relation, weight: 8}`. Weights skew load-phase traffic toward production-like shares; probing itself samples every target equally. |
| `probe.subject_types` | inferred terminal types | User types to sample probe subjects from. Inferred from the model when empty (usually correct). |
| `probe.samples_per_target` | `200` | Candidate samples per target before allowed/denied resampling. Raise to smooth per-target percentile variance; lower for a faster probe. |
| `probe.cohort_bias` | `0.85` | Probability the probe subject is drawn from the object's cohort. Higher = more allowed paths resolve; lower = more cross-tenant denials. |
| `probe.allowed_ratio` | `0.5` | Desired allowed/denied mix in the corpus. `0.5` = 50/50. Use `-1` to keep the natural mix observed during probing (recommended for absolute throughput numbers). |
| `probe.max_duplication` | `5` | Caps how far probe may duplicate scarce outcomes to hit `allowed_ratio`. `-1` removes the cap. Targets that would exceed it keep their natural mix, with a warning. |
| `probe.concurrency` | `8` | Concurrent probe workers. |
| `probe.attribute_ds_queries` | `false` | When `true` (requires `metrics.prometheus_url`), runs an extra probe pass that attributes **datastore queries per check, per relation**. See [Per-relation datastore-query attribution](#per-relation-datastore-query-attribution) below. |

### Choosing `allowed_ratio`

- **`0.5` (default)**: best for comparing relations against each other — same
  mix everywhere, so a relation's latency reflects its eval cost, not its
  allowed rate.
- **`-1` (natural mix)**: best for predicting production latency. Real
  callers don't get a balanced mix.
- **Production share**: if you know roughly what fraction of real checks
  allow, set `allowed_ratio` to match. Just be aware `max_duplication` may
  block extreme ratios.

### Per-relation datastore-query attribution

The server-side view (`metrics.prometheus_url`) reports *aggregate* datastore
queries per request across the whole measured phase — it can't tell you that
*this* relation costs 12 datastore reads while *that* one costs 1. Per-relation
cost is the sharpest capacity signal for spotting an expensive rewrite, and
`probe.attribute_ds_queries: true` measures it.

After building the corpus, fgaperf replays a small distinct batch of checks per
relation — **one relation at a time**, at `HIGHER_CONSISTENCY` so they bypass
the check cache and hit the datastore — and diffs OpenFGA's
`openfga_datastore_query_count` histogram around each batch. The per-batch
sum÷count is that relation's mean datastore queries per check, recorded on the
corpus and surfaced as a "DS queries/check (probe)" column in the findings
per-relation table (and `ds_queries_by_target` in the results JSON).

- Requires `metrics.prometheus_url`; fgaperf refuses to start without it.
- **Best-effort and approximate.** Values are histogram diffs, so concurrent
  traffic on a shared server pollutes them; run it against a dedicated server
  for clean numbers. A failed scrape leaves a relation unattributed rather than
  failing the probe.
- **Off by default.** When off, the probe path and corpus are byte-identical to
  a run without the flag.
- It reflects *probe-time* checks, not the measured load phase, and the column
  is omitted for `batch-check` (whose report rows mix relations).

## `replay` — corpus from a real check log

Set `corpus_source: replay` to build the corpus from a real check log instead
of synthesizing one from the model. Use this when you have an OpenFGA request
log or app audit trail and want the load mix to match *that* distribution
exactly, bypassing probe synthesis.

| Field | Default | Description |
|---|---|---|
| `replay.file` | _(required when `corpus_source: replay`)_ | Path to a JSONL check log. One JSON object per line: `{"user":..., "relation":..., "object":...}` with optional `contextual_tuples` (an array of `{user, relation, object[, condition]}`) and `context` (a CEL context map). Extra fields — store IDs, timestamps, consistency, etc. — are ignored, so a raw OpenFGA request log can be fed in directly. |

How replay differs from the synthesized probe:

- **Ground truth is still learned by probing.** Each *distinct* log entry is
  executed once at `HIGHER_CONSISTENCY`, exactly as the normal probe does for
  synthesized candidates (principle: outcomes are never predicted statically).
  `probe.concurrency` sets how many of these run in parallel.
- **The load mix follows the log.** The corpus is weighted by the log's natural
  per-target (`type#relation`) frequencies, so the load phase replays each
  target in proportion to how often it appeared. Within a target, distinct
  checks are picked uniformly.
- **Probe sampling and resampling are skipped.** `probe.targets`,
  `samples_per_target`, `cohort_bias`, `allowed_ratio`, and `max_duplication`
  do not apply — the log *is* the corpus. The `contextual` block is also unused;
  contextual tuples come straight from each log line.
- **The store must match the log.** The `user`/`object` IDs in the log have to
  exist in the seeded store for outcomes to be meaningful — run `setup` against
  the same model the log was captured from, or replay against a store seeded to
  match.
- **Malformed lines are skipped, not fatal.** Blank lines are ignored; lines
  that are not valid JSON, are missing `user`/`relation`/`object`, or whose
  `object` lacks a `type:id` prefix are counted and reported (with a few
  sample reasons), and the run continues.

```yaml
corpus_source: replay
replay:
  file: production-checks.jsonl
```

## `load` — the measured phase

| Field | Default | Description |
|---|---|---|
| `load.endpoint` | `check` | `check` (one tuple per HTTP request), `batch-check` (many per request), `list-objects` ("which objects of a type can this user access?"), or `list-users` ("which users can access this object?"). The list endpoints reuse the corpus's (user, relation, object) triples and add a result-set-size distribution to the findings; their `verify_results` is a best-effort spot-check (each entry's own object/user should appear in its own listing) that can false-positive under OpenFGA's result-cap truncation. |
| `load.transport` | `http` | Wire protocol for the **measured phase**: `http` (REST + JSON) or `grpc`. gRPC is OpenFGA's lower-overhead production path; switching removes the HTTP+JSON serialization cost from the client-side numbers, so the run measures closer to what the server actually costs. Setup and probe always use HTTP; only the load loop varies. gRPC dials `openfga.grpc_url` (default `host:8081`) over a single tuned connection. All four endpoints work over either transport. |
| `load.batch_size` | `20` | Tuples per `batch-check` request. Ignored for `check`. |
| `load.concurrency` | `16` | Parallel workers issuing requests. Cap by the concurrency of your real callers. |
| `load.client_id` | `0` | Distinct non-negative load-generator ID for distributed runs. It offsets the request-selection, poisson-arrival, and churn RNG streams so several clients can replay the same corpus without walking the same pseudo-random sequence. Set a unique value per process, or pass `-client-id N`; merge the resulting reports with `fgaperf merge`. |
| `load.rate` | `0` | Fixed offered requests/sec. `0` = closed loop. Mutually exclusive with `load.sweep`. |
| `load.warmup` | `10s` | Leading slice discarded so caches/connections steady-state before measurement. |
| `load.duration` | `60s` | Measured window after warmup. |
| `load.consistency` | `MINIMIZE_LATENCY` | `MINIMIZE_LATENCY` (uses caches) or `HIGHER_CONSISTENCY` (skips them). Pick what your production callers use. |
| `load.verify_results` | `false` (`true` in example) | Compare each load-time response against the probe-time ground truth and count mismatches. |
| `load.write_rate` | `0` | Background tuple writes per second during the measured phase. Lets you measure checks under realistic cache-invalidation pressure. `0` = read-only. |
| `load.sample_file` | unset | Dump one JSON line per measured sample (target, latency, response latency, outcome class, timestamp) for your own analysis. A `.gz` suffix gzips the stream; a sweep's steps all append to one file. Off when unset. |
| `load.sweep.rates` | empty | When set, run a multi-step sweep instead of a single rate. List of offered req/s, e.g. `[200, 500, 1000, 2000]`. Mutually exclusive with `load.rate`. |
| `load.sweep.step_duration` | `60s` | Measured window per sweep step. Warmup applies once at the start of the sweep, not per step. |
| `load.slo_p99` | unset | Optional target response-latency p99. A sweep step counts as "passing" only if response p99 stays under this. |

### Closed-loop vs fixed-rate

- **Closed-loop (`rate: 0`)**: workers issue back-to-back, no pacing. Tells
  you what the server can sustain when callers never wait. Useful for
  saturation tests; misleading for latency targets, because *all* latency
  rises with concurrency in closed loop.
- **Fixed-rate (`rate: N`)**: offer N req/s with a steady ticker. The report
  separately tracks **achieved rate** (what the server actually
  processed) and **response latency** (measured from the scheduled send
  time, so it includes queueing delay when the server falls behind). If
  achieved < 98% of offered, you've found a soft ceiling.
- **Sweep**: automates the rate search. The report headlines the
  **saturation knee** — the highest step that kept up (and stayed under
  `slo_p99` if set).

### Distributed load generation

When one fgaperf process is bounded by its host CPU or NIC, run several
generators against the same seeded store and corpus:

```bash
./fgaperf setup -config config.yaml
./fgaperf probe -config config.yaml

./fgaperf run -config config.yaml -client-id 1 -output-dir results/client-1
./fgaperf run -config config.yaml -client-id 2 -output-dir results/client-2
./fgaperf merge -output-dir results/merged results/client-*/results-*.json
```

Each client must use the same workload shape (`endpoint`, `transport`,
`warmup`, `duration`, corpus, and model/store), but should use a distinct
`load.client_id`. The merged report sums concurrency, offered/achieved rates,
throughput, errors, and mismatches, then merges latency percentiles from the
digest sketches embedded in each results JSON. Server-side Prometheus sections
are intentionally not merged, because several clients scraping the same shared
OpenFGA counters would double-count the same server traffic. Sweep reports are
not mergeable yet; merge one fixed-rate or closed-loop run at a time.

### `MINIMIZE_LATENCY` vs `HIGHER_CONSISTENCY`

OpenFGA caches Check responses. `MINIMIZE_LATENCY` uses those caches —
fast, but a fresh Write may take a moment to be reflected.
`HIGHER_CONSISTENCY` skips the caches and always re-evaluates — slow but
fresh. fgaperf's probe phase always uses `HIGHER_CONSISTENCY` (so probe
expectations don't drift with cache state), but load uses whatever you set
here. Pick the mode your real callers use:

- Most read paths → `MINIMIZE_LATENCY`.
- UI flows that re-check immediately after a Write → `HIGHER_CONSISTENCY`.
- A mix → run twice and compare with `fgaperf compare`.

### HTTP vs gRPC transport

`load.transport` chooses the wire protocol for the measured phase. HTTP (the
default) talks the REST + JSON API; gRPC talks OpenFGA's protobuf API over a
single multiplexed HTTP/2 connection. The server does the same authorization
work either way — what differs is the per-request serialization and framing
overhead the client pays, which is folded into every client-side latency
number.

Use gRPC when:

- Your real high-throughput callers use gRPC and you want numbers that match.
- You want to factor HTTP+JSON cost out of the client-side view to get closer
  to "what does the server actually cost per check".

To quantify the difference, run the same config twice (once per transport,
same `random_seed`) and `fgaperf compare` the two results — the client-side
percentiles drop by the serialization overhead while throughput rises
correspondingly in closed-loop mode. Setup and probe always use HTTP, so the
seeded store and corpus are identical across the two runs. The gRPC endpoint
is `openfga.grpc_url` (default `host:8081`, plaintext; set `openfga.grpc_tls`
for managed/cloud).

### When `verify_results` flags mismatches

A mismatch is a load-time response that differs from probe-time ground truth.
The model and graph cannot have changed, so the usual causes are:

1. **Cache staleness under churn** (`load.write_rate > 0` and
   `consistency: MINIMIZE_LATENCY`). Expected; informational. Switch
   consistency or look at the per-target rate.
2. **Probe ran against a different store / model.** Make sure `setup` ran
   in the same invocation (or `state_file` points at the right one).
3. **Server bug / config divergence.** Rare but the highest-value case to
   catch; the `mismatches-<stamp>.json` file enumerates every diverging
   check.

## `metrics` — server-side view

| Field | Default | Description |
|---|---|---|
| `metrics.prometheus_url` | unset | OpenFGA's Prometheus metrics endpoint (the docker-compose stack publishes `http://localhost:2112`). When set, fgaperf scrapes the endpoint at the start and end of the measured phase and reports the diff: request duration, datastore queries per check, dispatches, cache hit rate. On a shared OpenFGA deployment, these metrics may include unrelated traffic unless the server exposes labels you can isolate for this run. |

`Datastore queries per request` is the most portable capacity metric — it's
independent of network and JSON encoding, so it sizes the database without
needing identical client placement.

## `conditions` — CEL parameter generation

```yaml
conditions:
  <condition-name>:
    tuple_params: [<param>, ...]
    params:
      <param>:
        pool: <pool-name>
        keys: <int>                 # fixed size for map/list params
        keys_distribution:           # OR — drawn per tuple
          values:  [2, 12]
          weights: [0.9, 0.1]
```

- **`tuple_params`** lists which parameters are bound on the tuple (the
  rest are supplied on the request). Default: every map/list parameter is
  tuple-side, every scalar is request-side. Override when your model
  pattern differs.
- **`params.<name>.pool`** names the value pool to draw from (defined
  under `pools`). Default pool is `default`.
- **`params.<name>.keys`** sets a fixed entry count for map/list params.
- **`params.<name>.keys_distribution`** draws the entry count per tuple
  from a weighted discrete distribution. Real datasets skew — most maps
  small, a few huge; this lets a single run mix both. Empty `weights`
  means uniform.

## `pools` — value pools

```yaml
pools:
  scopes:
    prefix: "scope-"
    count: 16
    # OR
    values: [read, write, admin]
```

- **`prefix` + `count`**: generates `scope-00`, `scope-01`, ... .
- **`values`**: explicit list; overrides `prefix`/`count`.

A `default` pool of 16 `val-NN` values is provided automatically. Define
named pools when you want condition parameters to draw from a smaller or
domain-specific set (mimicking, say, a fixed set of OAuth scopes or
geographies).

---

## Reproducibility checklist

Two runs are directly comparable only when every observable input matches.
`fgaperf compare` enforces this by naming any config keys that differ; if you
want comparability:

- Same `random_seed`, same `model_file`.
- Same `seed.*` block.
- Same `load.endpoint`, `load.concurrency`, `load.warmup`,
  `load.duration`, `load.consistency`.
- Same OpenFGA version and datastore configuration (record these
  separately — fgaperf can't see them).
- Same client placement relative to OpenFGA (latency matters; running on
  the same host vs across a region is a different test).

---

## Regression baselines

Two commands turn a results JSON into a long-lived regression gate:

- `fgaperf baseline save <results.json>` writes a compact
  `baseline-<stamp>.json` (run shape, config fingerprint, random seed,
  throughput, key latency percentiles, per-target p99, and server-side
  datastore cost when present).
- `fgaperf compare -against-baseline <baseline.json> [-max-regression …]
  [-exit-on-regression=false] <results.json>` compares a fresh run to the
  baseline and exits non-zero when any tracked metric regresses past its
  threshold.

`-max-regression` is a comma-separated `metric=percent` list (default
`p99=10%,throughput=-5%`). Latency metrics (`mean`, `p50`, `p90`, `p95`, `p99`,
`max`) gate the max allowed increase overall and per-target; `throughput` gates
the max allowed decrease; `ds_queries` and `server_p99` gate server-side cost
when `metrics.prometheus_url` is set. Sign is normalized to "worse direction
allowed," so `throughput=5%` and `throughput=-5%` are equivalent. A gate that
can't be evaluated becomes a warning, not a silent pass. See
[ci-regression-gate.md](ci-regression-gate.md) for the CI recipe.
