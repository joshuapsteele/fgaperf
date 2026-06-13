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
| `output_dir` | `results` | Where `results-<stamp>.json`, `findings-<stamp>.md`, and (if any) `mismatches-<stamp>.json` are written. |
| `random_seed` | `0` (time-based) | Fixed seed makes generation, probing, and request ordering reproducible. Same seed + same config = same run. |
| `keep_store` | `false` | When `true`, `fgaperf all` does not delete the store at the end. Useful when iterating on probe/run without re-seeding. |

## `openfga` — server connection

| Field | Default | Description |
|---|---|---|
| `openfga.api_url` | `http://localhost:8080` | OpenFGA HTTP API base URL. |
| `openfga.store_name` | `fgaperf` | Name used when creating the store. `cleanup -all-stores` matches on this. |
| `openfga.api_token` | unset | Pre-shared API token when `OPENFGA_AUTHN_METHOD=preshared`. |
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

### Choosing `allowed_ratio`

- **`0.5` (default)**: best for comparing relations against each other — same
  mix everywhere, so a relation's latency reflects its eval cost, not its
  allowed rate.
- **`-1` (natural mix)**: best for predicting production latency. Real
  callers don't get a balanced mix.
- **Production share**: if you know roughly what fraction of real checks
  allow, set `allowed_ratio` to match. Just be aware `max_duplication` may
  block extreme ratios.

## `load` — the measured phase

| Field | Default | Description |
|---|---|---|
| `load.endpoint` | `check` | `check` (one tuple per HTTP request) or `batch-check` (many per request). |
| `load.batch_size` | `20` | Tuples per `batch-check` request. Ignored for `check`. |
| `load.concurrency` | `16` | Parallel workers issuing requests. Cap by the concurrency of your real callers. |
| `load.rate` | `0` | Fixed offered requests/sec. `0` = closed loop. Mutually exclusive with `load.sweep`. |
| `load.warmup` | `10s` | Leading slice discarded so caches/connections steady-state before measurement. |
| `load.duration` | `60s` | Measured window after warmup. |
| `load.consistency` | `MINIMIZE_LATENCY` | `MINIMIZE_LATENCY` (uses caches) or `HIGHER_CONSISTENCY` (skips them). Pick what your production callers use. |
| `load.verify_results` | `false` (`true` in example) | Compare each load-time response against the probe-time ground truth and count mismatches. |
| `load.write_rate` | `0` | Background tuple writes per second during the measured phase. Lets you measure checks under realistic cache-invalidation pressure. `0` = read-only. |
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
| `metrics.prometheus_url` | unset | OpenFGA's Prometheus metrics endpoint (the docker-compose stack publishes `http://localhost:2112`). When set, fgaperf scrapes the endpoint at the start and end of the measured phase and reports the diff: request duration, datastore queries per check, dispatches, cache hit rate. |

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
