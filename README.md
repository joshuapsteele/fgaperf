# fgaperf

Model-driven performance testing for OpenFGA.

Point fgaperf at a compiled OpenFGA authorization model and a running OpenFGA
server. It creates a coherent tuple graph, probes which checks are allowed or
denied, runs load, and writes a findings document with latency broken down by
relation, CEL-conditioned paths, and contextual-tuple requests.

fgaperf is a small Go binary that talks to the OpenFGA HTTP API directly. It is
meant for realistic model-specific testing, where generic load tools do not
know enough about your authorization graph to generate useful checks.

## Installation Prerequisites

You need:

| Requirement | Notes |
|---|---|
| Go | Used to build fgaperf locally. The required version is in `go.mod`. |
| Docker-compatible runtime | Needed only if you want to use this repo's local OpenFGA/Postgres stack. Docker Desktop, Colima, Rancher Desktop, or another Docker-compatible runtime is fine. The Docker CLI must be able to reach a running container engine. |
| Docker Compose | The example stack uses `docker compose up -d`. If your system only provides `docker-compose`, use that equivalent command. |
| OpenFGA server | Either run the included Docker Compose stack, or point `openfga.api_url` at an existing OpenFGA deployment. |
| Compiled model JSON | Use the example model, or export your own with the OpenFGA CLI. |

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

`all` runs setup, probe, and load, then deletes the store it created. Results
are written to:

```text
results/results-<stamp>.json
results/findings-<stamp>.md
```

For a shorter smoke test, override the run duration:

```bash
sed -e 's/warmup: 10s/warmup: 2s/' -e 's/duration: 60s/duration: 8s/' \
  examples/config.yaml > /tmp/fgaperf-smoke.yaml
./fgaperf all -config /tmp/fgaperf-smoke.yaml
```

## Commands

```bash
./fgaperf inspect -config examples/config.yaml   # print model analysis, no server needed
./fgaperf setup   -config examples/config.yaml   # create store, write model, seed tuples
./fgaperf probe   -config examples/config.yaml   # build corpus.json
./fgaperf run     -config examples/config.yaml   # run load, write results/
./fgaperf cleanup -config examples/config.yaml   # delete the recorded store
```

Use `all` for normal one-shot runs. Use the separate phases when you want to
re-run load against the same seeded store. Set `keep_store: true` or pass
`-keep` if you do not want `all` to delete the store at the end.

## Using Your Own Model

fgaperf expects a compiled OpenFGA model JSON file. If you author models in the
OpenFGA DSL, export one with:

```bash
fga model transform --file model.fga > model.json
```

Then create a config with at least:

```yaml
model_file: model.json

openfga:
  api_url: http://localhost:8080
```

Run:

```bash
./fgaperf inspect -config config.yaml
```

`inspect` shows the inferred types, relations, assignable relations,
CEL-reachable relations, contextual relations, and condition parameter split.
Use that output to choose probe targets and tune the generated graph.

## Configuration

Every config field has a default. `examples/config.yaml` shows the full shape.
The most important knobs are:

| Knob | Effect |
|---|---|
| `seed.instances`, `seed.fanout` | Size and density of the generated graph. |
| `seed.cohorts` | Tenant-like partitions; tuples are biased within cohorts so intersections can resolve. |
| `contextual.relations` | Direct relations supplied per request as contextual tuples instead of persisted seed tuples. |
| `contextual.attach_probability` | Probability a sampled check carries contextual tuples. Use `1.0` when every production request carries context, or less than `1.0` to include denied missing-context paths. |
| `probe.targets` | Relations to measure. Omit to probe all relations. |
| `probe.allowed_ratio` | Desired allowed/denied mix in the corpus. Use `-1` to keep the natural mix. |
| `load.endpoint` | `check` or `batch-check`. |
| `load.rate` | Fixed offered requests/sec. `0` means closed-loop saturation testing. |
| `load.consistency` | `MINIMIZE_LATENCY` or `HIGHER_CONSISTENCY`. |
| `conditions`, `pools` | Tuple-side and request-side CEL condition context generation. |
| `random_seed` | Makes generated data and probes repeatable. |

## How It Works

Setup reads the model metadata, creates instances for each type, and writes
direct tuples that match the model. Instances are grouped into cohorts so deep
paths and intersections have a meaningful chance of resolving to allowed.

Probe samples candidate checks, executes each once with `HIGHER_CONSISTENCY`,
and records the observed result in `corpus.json`. This avoids trying to
statically predict outcomes through usersets, intersections, conditions, and
contextual tuples.

Run replays the corpus with configurable concurrency and rate. With
`verify_results: true`, fgaperf counts any response that differs from the
probe-time result as a mismatch.

## Reading Results

The generated findings document includes:

| Section | Use |
|---|---|
| Headline results | Overall throughput and latency. |
| CEL-conditioned paths | Checks whose resolution can evaluate a CEL condition. |
| Contextual tuples | Checks sent with request-local contextual tuples. |
| Per-relation breakdown | The most useful place to compare similar paths. |
| Write path | Tuple seeding throughput. |

Closed-loop runs are useful for finding saturation. Fixed-rate runs are better
for estimating latency at a realistic offered load.

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

## License

Apache-2.0. See [LICENSE](LICENSE).
