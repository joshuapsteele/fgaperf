# fgaperf

Model-driven performance testing for [OpenFGA](https://openfga.dev/docs/fga).

Point fgaperf at a compiled OpenFGA [authorization model](https://openfga.dev/docs/concepts#what-is-an-authorization-model)
and a running OpenFGA server. It creates a coherent
[tuple](https://openfga.dev/docs/concepts#what-is-a-relationship-tuple) graph,
probes which [Check](https://openfga.dev/docs/interacting/relationship-queries#check)
calls are allowed or denied, runs load, and writes a findings document with
latency broken down by relation, [CEL-conditioned](https://openfga.dev/docs/modeling/conditions)
paths, and [contextual-tuple](https://openfga.dev/docs/interacting/contextual-tuples)
requests.

fgaperf is a small Go binary that talks to the OpenFGA HTTP API directly. It is
meant for realistic model-specific testing, where generic load tools do not
know enough about your authorization graph to generate useful checks.

> New to OpenFGA or load testing? Skim the [Glossary](#glossary) at the bottom
> first; it defines every term used in the rest of this document and in the
> generated findings.

## Installation Prerequisites

You need:

| Requirement | Notes |
|---|---|
| Go | Used to build fgaperf locally. The required version is in `go.mod`. |
| Docker-compatible runtime | Needed only if you want to use this repo's local OpenFGA/Postgres stack. Docker Desktop, Colima, Rancher Desktop, or another Docker-compatible runtime is fine. The Docker CLI must be able to reach a running container engine. |
| Docker Compose | The example stack uses `docker compose up -d`. If your system only provides `docker-compose`, use that equivalent command. |
| OpenFGA server | Either run the included Docker Compose stack, or point `openfga.api_url` at an existing [OpenFGA](https://openfga.dev/docs/getting-started) deployment. |
| Compiled model JSON | Use the example model, or export your own with the [OpenFGA CLI](https://openfga.dev/docs/getting-started/cli). |

For example, a lightweight macOS setup with Colima is:

```bash
brew install go docker docker-compose colima
colima start --cpu 4 --memory 8 --disk 40
docker context use colima
docker info
```

`docker info` must succeed before `docker compose up -d` will work.

## Quick Start

Start the local Postgres-backed OpenFGA stack:

```bash
docker compose up -d
```

Build and run the example:

```bash
go build -o fgaperf .
./fgaperf all -config examples/config.yaml
```

`all` runs setup, probe, and load, then deletes the
[store](https://openfga.dev/docs/concepts#what-is-a-store) it created. Results
are written to:

```text
results/results-<stamp>.json
results/findings-<stamp>.md
```

For a shorter smoke test, override the run duration on the command line — no
config edit needed:

```bash
./fgaperf all -config examples/config.yaml -warmup 2s -duration 8s
```

The common load knobs are available as flags that override the config after it
loads: `-duration`, `-warmup`, `-rate`, `-concurrency`, `-endpoint`,
`-consistency`, and `-output-dir`. Overrides are recorded in the results JSON's
`resolved_config`, so a run stays reproducible from its output alone. For
example, sweep a single rate point against the example store:

```bash
./fgaperf run -config examples/config.yaml -rate 2000 -duration 30s
```

## Commands

```bash
./fgaperf inspect -config examples/config.yaml   # print model analysis, no server needed
./fgaperf setup   -config examples/config.yaml   # create store, write model, seed tuples
./fgaperf probe   -config examples/config.yaml   # build corpus.json
./fgaperf run     -config examples/config.yaml   # run load, write results/
./fgaperf cleanup -config examples/config.yaml   # delete the recorded store

# generate an annotated starter config from a compiled model:
./fgaperf gen-config -model model.json > config.yaml

# diff two runs (latency deltas, server-side deltas, config differences):
./fgaperf compare -config examples/config.yaml results/results-A.json results/results-B.json
```

Use `all` for normal one-shot runs. Use the separate phases when you want to
re-run load against the same seeded store (for example, re-running `run` with
a different `load.rate` against the corpus the `probe` phase already built).
Set `keep_store: true` or pass `-keep` if you do not want `all` to delete the
store at the end.

`compare` takes two results JSON files and writes a `compare-<stamp>.md` that
tables the latency and server-side deltas, names every config key that
differed between the runs, and calls out anything that makes the comparison
apples-to-oranges (different endpoint, corpus size, duration, or concurrency).

## Using Your Own Model

fgaperf expects a compiled OpenFGA model JSON file. If you author models in the
[OpenFGA DSL](https://openfga.dev/docs/modeling/getting-started), export one
with:

```bash
fga model transform --file model.fga > model.json
```

Then either create a config with at least:

```yaml
model_file: model.json

openfga:
  api_url: http://localhost:8080
```

or scaffold a starting point straight from the model (every section is
annotated; instance counts, fanouts, contextual guesses, and conditions/pools
blocks are picked from the model's shape):

```bash
./fgaperf gen-config -model model.json > config.yaml
```

`gen-config` writes to stdout by default; pass `-o config.yaml` to write to a
file (refuses to clobber unless `-force` is given). The output is a starting
point — review the contextual guesses, condition pools, and probe target list,
then tune cohorts/instances/fanout to the scale of the system you're modeling.

Run:

```bash
./fgaperf inspect -config config.yaml
```

`inspect` prints what fgaperf learned from your model: every relation, whether
it accepts direct tuples ("assignable"), whether any path through it can
evaluate a [CEL condition](https://openfga.dev/docs/modeling/conditions),
whether it is treated as
[contextual](https://openfga.dev/docs/interacting/contextual-tuples) (supplied
per request rather than persisted), the inferred terminal subject types (the
user types reached at the bottom of the graph), and the tuple-side vs
request-side split of each condition's parameters. Use that output to choose
probe targets and tune the generated graph.

## Configuration

Every config field has a default. `examples/config.yaml` shows the full shape
with every knob annotated, and
[docs/configuration-reference.md](docs/configuration-reference.md) is the
long-form reference. The most important knobs are:

| Knob | Effect |
|---|---|
| `seed.instances`, `seed.fanout` | Size and density of the generated graph. *Fanout* is the number of tuples written per (object, relation, user type) — e.g. fanout 4 on `document#editor` means each document gets 4 editor tuples. Fanout keys take an optional `@usertype` suffix to set fanout per accepted user type (`document#editor@user: 0`, `document#editor@group#member: 4`); the bare key stays the default for the rest. |
| `seed.cohorts` | Tenant-like partitions; tuples are biased to link within the same cohort so [intersection](https://openfga.dev/docs/modeling/building-blocks/usersets#the-intersection-operator) and [tuple-to-userset](https://openfga.dev/docs/modeling/building-blocks/object-to-object-relationships) relations can actually resolve. Set roughly to the number of tenants you want to simulate. |
| `seed.wildcard_probability`, `seed.wildcard_probabilities` | Chance each object gets a [public/wildcard](https://openfga.dev/docs/modeling/public-access) (`user:*`) tuple where the model allows one; the plural form overrides it per `type#relation`. |
| `contextual.relations` | Direct relations supplied per request as [contextual tuples](https://openfga.dev/docs/interacting/contextual-tuples) instead of persisted seed tuples. Use this for facts that live in the request, like an OAuth scope or a "currently active session" flag. |
| `contextual.attach_probability` | Probability a sampled check carries contextual tuples. Use `1.0` when every production request carries context, or less than `1.0` to include denied missing-context paths. |
| `probe.targets` | Relations to measure. Omit to probe all relations. Entries are bare strings or `{relation: document#viewer, weight: 8}`; weights skew the load phase's traffic mix toward production-like shares. |
| `probe.allowed_ratio` | Desired allowed/denied mix in the corpus. `0.5` means a 50/50 split; use `-1` to keep the natural mix the probe observed. |
| `probe.max_duplication` | Caps how far probe may duplicate scarce outcomes to hit `allowed_ratio` (default `5`, `-1` = unbounded). Targets that would exceed it keep their natural mix, with a warning. |
| `load.endpoint` | [`check`](https://openfga.dev/docs/interacting/relationship-queries#check) (one tuple per request) or [`batch-check`](https://openfga.dev/docs/interacting/relationship-queries#batch-check) (many per request). |
| `load.rate` | Fixed *offered* requests/sec. `0` means closed-loop: workers issue the next request as soon as the previous one returns (used to find max throughput). |
| `load.sweep.rates`, `load.slo_p99` | Step through several offered rates in one run to find the **saturation knee** — the highest rate the server sustained (achieved ≥ 98% of offered, and response-latency p99 under `slo_p99` when set). Mutually exclusive with `load.rate`. |
| `load.write_rate` | Background tuple writes/sec during the measured phase, so checks run against a churning store instead of the read-only best case. Churn tuples only ever link fresh churn-only instances, so `verify_results` stays meaningful. |
| `load.consistency` | [`MINIMIZE_LATENCY`](https://openfga.dev/docs/interacting/consistency) (cached, fast, may be stale) or `HIGHER_CONSISTENCY` (skips caches; slower, fresh). |
| `metrics.prometheus_url` | OpenFGA's [Prometheus metrics](https://openfga.dev/docs/getting-started/setup-openfga/configuration) endpoint (the compose stack publishes `http://localhost:2112`). When set, results gain a server-side view: request duration, datastore queries per check, dispatches, cache hit rate — diffed over the measured phase only. |
| `conditions`, `pools` | Tuple-side and request-side [CEL condition](https://openfga.dev/docs/modeling/conditions) context generation. A param's `keys` may be replaced with `keys_distribution: {values: [2, 12], weights: [0.9, 0.1]}` to draw map/list sizes per tuple (most maps small, some big — closer to real data). |
| `random_seed` | Makes generated data and probes repeatable. Same seed + same config = same tuple graph and corpus. |

Configs are validated strictly: unknown keys, out-of-range values, and names
that do not exist in the loaded model all fail fast with an error naming the
bad key, rather than silently running with defaults.

## How It Works

fgaperf runs in three phases, each producing artifacts the next phase consumes.

**Setup.** Reads the model metadata, creates instances of each type, and
writes the direct tuples allowed by the model. Instances are grouped into
*cohorts* (tenant-like partitions) and tuples are biased to link within the
same cohort, so deep paths and intersections have a meaningful chance of
resolving to "allowed" instead of randomly missing.

**Probe.** Samples candidate checks `(user, relation, object)`, executes each
one once against the server with `HIGHER_CONSISTENCY` (no caches), and
records the observed allowed/denied result in `corpus.json`. This avoids
trying to statically predict outcomes through
[usersets](https://openfga.dev/docs/modeling/building-blocks/usersets),
intersections, CEL conditions, and contextual tuples — which is a tar pit even
for small models.

**Run.** Replays the corpus under load with configurable concurrency and
rate. With `verify_results: true` (default), fgaperf counts any response that
differs from the probe-time result as a *mismatch* (usually caching or
consistency issues, since the model and graph cannot have changed between
probe and run).

## Reading Results

Every run writes a JSON file (machine-readable) and a `findings-<stamp>.md`
document (human-readable) into `results/`. The findings document includes
inline blurbs under each section explaining what it measures and how to read
bad numbers; a "How to read this" legend at the bottom defines every column.
The sections are:

| Section | Use |
|---|---|
| Headline results | Overall throughput and latency, computed over the actual measured window. |
| CEL-conditioned paths | Checks whose resolution can evaluate a CEL condition. |
| Contextual tuples | Checks sent with request-local contextual tuples. |
| Per-relation breakdown | The most useful place to compare similar paths, including per-relation error counts. |
| Rate sweep | One row per swept rate (achieved rate, latency, response p99, datastore queries/request) with the saturation knee marked. Only present for sweep runs. |
| Server-side view | OpenFGA's own Prometheus metrics diffed over the measured phase: request duration, datastore queries and dispatches per check, cache hit rate. Only present when `metrics.prometheus_url` is set. |
| Background tuple writes | Latency of the churn write/delete calls, plus a note that the check populations were measured under that write rate. Only present when `load.write_rate` is set. |
| Errors | Error counts by class (timeout, connection, 4xx, 5xx, decode) with the first few verbatim messages. |
| Write path | Tuple seeding throughput. |

**Closed-loop vs fixed-rate runs.** Closed-loop runs (`load.rate: 0`) issue
the next request as soon as the previous one returns, with no pacing; they
are useful for finding *saturation* throughput. Fixed-rate runs hold the
offered rate steady and are better for estimating latency at a realistic
production load. Fixed-rate findings also report the *achieved* rate against
the offered rate and a separate **response latency** row measured from each
request's scheduled send time; when the server cannot keep up, that row — not
service latency — is what callers would experience (this is the
"coordinated-omission" corrected view). A `load.sweep` run automates the rate
search: it steps through the configured rates against the same store and
corpus and headlines the **knee**, the last offered rate the server kept up
with.

Results JSON embeds the environment and the full resolved config (credentials
redacted), so any results file plus its `random_seed` is enough to reproduce
the run — and enough for `compare` to name exactly which knobs differed
between two runs.

## Docker Notes

The included `docker-compose.yaml` starts Postgres and OpenFGA v1.17.1 on:

| Port | Service |
|---|---|
| `8080` | OpenFGA HTTP API |
| `8081` | OpenFGA gRPC |
| `2112` | OpenFGA Prometheus metrics |
| `5432` | Postgres |

Local Docker runs are good for smoke tests and relative comparisons. For
publishable performance numbers, run fgaperf near the OpenFGA and datastore
deployment you actually care about, and keep client placement, datastore,
cache settings, OpenFGA version, and config constant across comparisons.

## Glossary

Terms used throughout the docs, config, and findings output. Links go to the
[upstream OpenFGA documentation](https://openfga.dev/docs/fga) where deeper
background helps.

### OpenFGA concepts

- **Authorization model** — the schema that declares your types, relations,
  and how they combine. fgaperf needs the *compiled* JSON form (export with
  `fga model transform`). [Docs](https://openfga.dev/docs/concepts#what-is-an-authorization-model).
- **Store** — a single OpenFGA tenant: one model, one tuple set, one set of
  reads/writes. fgaperf creates a store per run by default and deletes it at
  the end. [Docs](https://openfga.dev/docs/concepts#what-is-a-store).
- **Tuple** (relationship tuple) — a fact like `user:alice is editor of
  document:42`. Writing tuples is how you grant access; reading them
  (indirectly, via Check) is how you enforce it. [Docs](https://openfga.dev/docs/concepts#what-is-a-relationship-tuple).
- **Relation** — a named edge type in the model (e.g. `viewer`, `editor`,
  `parent`). A relation can be assigned directly, derived from other
  relations (a *userset* or *tuple-to-userset*), or computed via boolean
  combinations.
- **Userset** — a relation whose users are themselves defined by another
  relation, e.g. "viewers of a document include `group#member` for any
  assigned group". [Docs](https://openfga.dev/docs/modeling/building-blocks/usersets).
- **Tuple-to-userset** — "follow this relation, then evaluate that relation
  on what you find" — e.g. a document's viewers include the viewers of its
  parent folder. [Docs](https://openfga.dev/docs/modeling/building-blocks/object-to-object-relationships).
- **Intersection / exclusion** — relations defined as `a AND b` or `a BUT
  NOT b`. Intersections are why fgaperf groups tuples into cohorts: a
  random user almost never satisfies two independent paths, so naive
  seeding would produce a corpus that is 100% denied.
- **Wildcard / public access** — a tuple like `user:* is viewer of document:42`
  that grants the relation to everyone of that user type. [Docs](https://openfga.dev/docs/modeling/public-access).
- **CEL condition** — a [Common Expression Language](https://github.com/google/cel-spec)
  predicate attached to a tuple, evaluated against parameters supplied on
  the tuple (tuple-side) and on the request (request-side). Conditions let
  authorization depend on values like "current time" or "OAuth scope".
  [Docs](https://openfga.dev/docs/modeling/conditions).
- **Contextual tuple** — a tuple supplied with the Check request rather
  than persisted in the store. Used for facts that only matter for one
  request (e.g. "the caller's session is currently active"). [Docs](https://openfga.dev/docs/interacting/contextual-tuples).
- **Check** — the read endpoint that answers "can `user` do `relation` on
  `object`?". fgaperf's primary measurement target. [Docs](https://openfga.dev/docs/interacting/relationship-queries#check).
- **BatchCheck** — same question, many tuples per HTTP request. Trades
  per-request overhead for higher per-tuple throughput when callers
  naturally batch. [Docs](https://openfga.dev/docs/interacting/relationship-queries#batch-check).
- **Consistency: `MINIMIZE_LATENCY` vs `HIGHER_CONSISTENCY`** — `MINIMIZE_LATENCY`
  reads from caches when it can; `HIGHER_CONSISTENCY` skips caches and is
  slower but always fresh. Choose whichever your production callers use.
  [Docs](https://openfga.dev/docs/interacting/consistency).

### fgaperf concepts

- **Cohort** — a tenant-like partition that fgaperf imposes on the generated
  instances. Tuples link inside cohorts most of the time, so deep paths and
  intersections resolve to "allowed" with realistic frequency.
- **Fanout** — tuples per (object, relation, allowed user type). Fanout 4 on
  `document#editor` writes 4 editor tuples per document.
- **Corpus** — the list of `(user, relation, object)` checks the load phase
  replays. Built by the probe phase, stored in `corpus.json`.
- **Probe** — the one-by-one execution of candidate checks that establishes
  ground truth (allowed / denied) for each corpus entry. Always uses
  `HIGHER_CONSISTENCY` so probe expectations are not contaminated by stale
  caches.
- **Mismatch** — under load, a check whose allowed/denied result differs
  from what probe recorded. Almost always points at cache staleness or
  consistency settings, not at fgaperf itself.

### Load-testing concepts

- **p50 / p90 / p95 / p99** — latency percentiles. p99 = 99% of requests
  were faster than this value. Tail latency (p99 and above) typically
  drives user-visible pain.
- **Throughput** — completed requests per second over the measured window.
- **Closed-loop** — workers issue requests back-to-back with no pacing.
  Useful for finding the maximum the server can sustain, but does not
  represent real callers.
- **Offered rate** — requests per second the load generator *tried* to
  send. Set with `load.rate`.
- **Achieved rate** — requests per second the server actually processed.
  Less than offered = the server fell behind.
- **Service latency** — measured from "request leaves the client" to
  "response arrives". The default percentile rows.
- **Response latency** — measured from each request's *scheduled* send time.
  When the server falls behind, response latency includes the queueing
  delay that callers experience but service latency hides. Sometimes called
  the "coordinated-omission corrected" view.
- **Saturation knee** — the highest offered rate the server still keeps up
  with. Beyond the knee, achieved rate plateaus and response-latency p99
  rises sharply.
- **SLO p99** — a target tail latency. With `load.slo_p99` set, sweep steps
  count as "passing" only if they keep response p99 below that target.
- **Warmup** — a leading slice of the run whose samples are discarded so
  JIT-equivalent effects (cache fill, connection pool warmup) do not skew
  measurements.
- **Datastore queries per request** — how many database round-trips OpenFGA
  made per Check. The capacity currency for OpenFGA sizing: it stays
  meaningful across network conditions, JSON encoding, etc.

## License

Apache-2.0. See [LICENSE](LICENSE).
