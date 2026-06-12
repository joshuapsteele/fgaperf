# fgaperf

Model-driven performance testing for OpenFGA. Point it at a compiled
authorization model (`.json`) and a running OpenFGA instance; it generates a
coherent tuple graph from the model, discovers which checks resolve to
allowed or denied, runs a measured load phase, and writes a findings document
with CEL condition overhead and contextual-tuple requests reported separately
from baseline paths. When it's done, it deletes the store it created.

OpenFGA's own benchmarks are in-process Go benchmarks for datastore
development, and generic load tools (k6, vegeta) have no concept of an
authorization model. fgaperf is a single static Go binary with one dependency
(yaml parsing) that talks to the plain HTTP API, deliberately avoiding SDK
middleware between the measurement and the wire.

## Quick start

    docker compose up -d        # Postgres-backed OpenFGA on :8080
    go build -o fgaperf .
    ./fgaperf all -config examples/config.yaml

`all` runs three phases in sequence — setup, probe, load — and then deletes
the test store, so repeated runs against a deployed OpenFGA leave nothing
behind. The store is also deleted if a phase fails or the run is
interrupted. Pass `-keep` (or set `keep_store: true`) to skip deletion, and
the phases can then be run independently, which is useful for re-running
load against an already-seeded store:

    ./fgaperf inspect -config examples/config.yaml   # print model analysis, no server needed
    ./fgaperf setup   -config examples/config.yaml   # create store, write model, seed tuples
    ./fgaperf probe   -config examples/config.yaml   # build the check corpus
    ./fgaperf run     -config examples/config.yaml   # measured load, writes results/
    ./fgaperf cleanup -config examples/config.yaml   # delete the store when finished

Setup records the store and model IDs in `.fgaperf-state.json`. Probe writes
`corpus.json`. Run writes `results/results-<stamp>.json` (machine-readable)
and `results/findings-<stamp>.md` (a findings document with the numbers
filled in, ready to extend with interpretation; see
`docs/example-findings.md`). Cleanup deletes the store named in the state
file; `cleanup -all-stores` instead deletes every store whose name matches
`openfga.store_name`, which recovers from any accumulation of leftover test
stores.

## Using your own model

The tool is model-agnostic: it works from any compiled OpenFGA authorization
model JSON, with no knowledge of your types or relations baked in. Export
one from your `.fga` DSL sources with the [FGA CLI](https://github.com/openfga/cli):

    fga model transform --file model.fga > model.json

Point `model_file` at it and run `fgaperf inspect` to see what the tool
derived: every (type, relation) pair, which ones accept direct tuples, which
ones can involve CEL evaluation, and how each condition's parameters will be
split between tuple-side and request-side context. Every config field has a
default, so a minimal config is just the model path; start there, look at
the inspect output, then add per-relation fanouts and probe targets as
needed. `examples/config.yaml` shows the full set of knobs tuned for the
example document-sharing model.

## How it works

Setup parses the model to learn which (type, relation) pairs accept direct
tuples, which user types each accepts (including userset references like
`group#member`, typed wildcards, and condition attachments), and what
parameters each CEL condition declares. Instances of every type are
partitioned into cohorts, which you can read as tenants. Tuples link
instances within the same cohort, which is what makes deep resolution paths
and intersection relations (in the example model, `document#can_share`
requires the same user to be directly shareable, a viewer, and active in the
request context) actually resolve to allowed for some subjects. Purely random
tuple graphs almost never satisfy intersections. Self-referential relations
like `folder#parent` only link from higher to lower instance indices, so the
generated graph is acyclic by construction. Writes go through parallel
workers in transactional batches of up to 100 (the server's default cap), and
the write rate is itself reported as a secondary measurement.

For conditioned direct types, the tool generates tuple-side condition
context at write time and request-side context at check time. By default,
structured parameters (maps, lists) are bound on the tuple and scalars come
from the request: in the example model, each conditioned tuple carries a
`granted_scopes` map drawn from a configurable value pool, and each check
supplies a `required_scope` drawn from the same pool. Pool size and
keys-per-map control the rate at which the CEL expression evaluates true
(the example config gives roughly 25%). All of this is overridable per
condition in config.

Contextual tuple relations are declared under `contextual.relations` as
`type#relation` keys. Those direct tuples are skipped during setup and instead
stored per corpus entry, then sent on Check or BatchCheck as
`contextual_tuples`. For a same-type gate like the example
`document#active_context`, the contextual tuple uses the checked object. For a
tenant-context pattern, where the contextual relation lives on a related
object such as a customer or account, fgaperf first looks for a real seeded
edge from the checked object to that related object type and uses that object
ID. `contextual.attach_probability` controls how often sampled checks carry
the assertion, which lets one corpus include both asserted and missing-context
paths.

Probe samples candidate (subject, relation, object) triples for each target
relation, biased toward the object's cohort, and executes each once with
HIGHER_CONSISTENCY to classify it. The resulting corpus is resampled to a
configurable allowed/denied mix (default 50/50). This sidesteps the need to
statically predict outcomes through intersections and conditions, and gives
the load phase ground truth: with `verify_results: true`, any response that
differs from its probe-time expectation is counted as a mismatch, which
surfaces cache-staleness questions directly.

Run replays the corpus from N workers, either closed-loop or at a fixed
offered rate (`rate: 100`). Use closed-loop to find the saturation
throughput of a deployment and fixed-rate to measure latency at a realistic
offered load; closed-loop latencies at saturation are dominated by queueing
and should not be quoted as service latency. Warmup samples are discarded.

## CEL overhead isolation

The tool computes statically, from the relation rewrite graph, whether each
relation's resolution can touch a conditioned tuple, and tags every check
accordingly. For the example model this splits cleanly: `document#viewer`
and `document#can_share` are CEL-reachable because a document can carry a
conditioned `user:*` viewer tuple, while `document#editor`, `folder#viewer`,
and `group#member` are unconditioned. The report breaks latency out by this
tag and by individual relation.

Read the conditioned/unconditioned delta as an upper bound: the two
populations differ in graph depth as well as condition evaluation. Two
sharper experiments are built in. First, compare per-relation numbers across
runs where the only change is condition complexity (edit the CEL expression
in a model variant). Second, OpenFGA exposes
`OPENFGA_MAX_CONDITION_EVALUATION_COST` and per-request condition metrics on
:2112 (`openfga_condition_evaluation_cost`,
`openfga_condition_compilation_duration_ms`,
`openfga_condition_evaluation_duration_ms`); scraping those during a run
attributes server-side CEL time precisely, where fgaperf measures the
end-to-end effect a caller sees.

## Contextual tuple isolation

The report also splits checks that carry contextual tuples from checks that do
not. This matters because contextual tuples are request payload, are evaluated
in-memory for that request, and participate in OpenFGA's check cache key. A
benchmark that persists a session- or tenant-context relation during setup can
therefore overstate production cacheability. Configure those relations under
`contextual.relations` instead, then use `attach_probability` to include both
allowed and denied context paths in the same run.

## Configuration reference

See the comments in `examples/config.yaml`. The knobs that matter most:

| Knob | Effect |
|---|---|
| `seed.instances`, `seed.fanout` | Graph size and shape; scale these up to test datastore behavior at realistic tuple counts |
| `seed.cohorts` | Tenant count; subjects and objects correlate within a cohort |
| `contextual.relations` | Direct relations supplied as per-request contextual tuples instead of persisted seed tuples |
| `contextual.attach_probability` | Probability a sampled check carries contextual tuples (default 1 when contextual relations are configured) |
| `probe.targets` | Which relations to measure; omit to probe everything |
| `probe.allowed_ratio` | Allowed/denied mix in the corpus (-1 keeps the natural mix) |
| `load.rate` vs closed loop | Latency-at-offered-load vs saturation throughput |
| `load.endpoint` | `check` or `batch-check` (server default: 50 items max per batch) |
| `load.consistency` | MINIMIZE_LATENCY vs HIGHER_CONSISTENCY; only differs when the check query cache is enabled server-side |
| `conditions`, `pools` | Tuple-side vs request-side condition parameters and value pools |
| `keep_store` | Skip the automatic store deletion at the end of `all` |
| `random_seed` | Full determinism of generation; vary it across runs to confirm stability |

## Operational notes

Authentication: set `openfga.api_token` for pre-shared key deployments.
The tool is a single static binary, so running a test configuration inside a
cluster as a Kubernetes Job requires only a scratch container image with the
binary, the model file, and a config.

Results are only comparable when client and server placement, datastore,
cache configuration, and OpenFGA version are held constant; the findings
document records the client side, and the server side belongs in the same
document when publishing numbers.

## License

Apache-2.0. See [LICENSE](LICENSE).
