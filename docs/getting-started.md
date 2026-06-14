# Getting Started

This walkthrough is for someone who has an OpenFGA model and wants the first
useful performance result without learning every fgaperf knob up front. The
README remains the reference; this page is the shortest path through a real
run.

## What fgaperf Measures

fgaperf generates a tuple graph from your compiled OpenFGA model, probes real
Check results to learn allowed/denied ground truth, then replays that corpus
under load. The generated findings report client-side latency, throughput,
per-relation hot spots, CEL-conditioned paths, contextual tuples, optional
OpenFGA Prometheus metrics, and whether load-time answers matched probe-time
answers. If any term is unfamiliar, the README glossary defines it.

## Bring Up OpenFGA

The bundled stack runs OpenFGA with Postgres and exposes the HTTP API on
`localhost:8080` and metrics on `localhost:2112`:

```bash
docker compose up -d
docker compose ps
```

Build the CLI:

```bash
go build -o fgaperf .
```

Run the pre-flight checklist before the first measurement:

```bash
./fgaperf doctor -config examples/config.yaml
```

If this fails, fix it before continuing. Most first-run failures are simply a
stopped compose stack, a port mismatch, or an auth setting mismatch.

## Inspect the Example Model

```bash
./fgaperf inspect -config examples/config.yaml
```

The output lists every `type#relation`. `[assignable]` means fgaperf can seed
direct tuples for that relation. `CEL` means some resolution path can evaluate
a condition. `[contextual]` means fgaperf will attach request-scoped tuples
instead of persisting that relation during setup.

For tools, the same analysis is available as JSON:

```bash
./fgaperf inspect -config examples/config.yaml -json
```

## Run a Smoke Test

Start with a short run:

```bash
./fgaperf all -config examples/config.yaml -warmup 2s -duration 8s
```

`all` creates a fresh store, writes the model, seeds tuples, probes the corpus,
runs load, writes `results/results-<stamp>.json` and
`results/findings-<stamp>.md` plus `results/report-<stamp>.html`, then deletes
the store. While attached to a terminal, setup, probe, and load print live
progress. When output is piped or CI captures logs, the progress lines are
suppressed.

## Read the Findings

Open the newest `results/report-<stamp>.html` for the visual version, or
`results/findings-<stamp>.md` for a copy/pasteable Markdown summary. The
Summary paragraph gives sustained throughput, p99, mismatches, and any major
callout. The Headline results table gives aggregate latency. The Per-relation
table is where to find hot relations. The Server-side view, when metrics are
reachable, separates OpenFGA's own duration and datastore-query cost from
client/network overhead.

Zero mismatches means load-time responses matched probe-time ground truth. A
nonzero mismatch count is not automatically a server bug; with
`MINIMIZE_LATENCY` and write churn, stale-cache reads can be expected.

## Tune Once and Compare

Try a denser tenant graph by increasing cohorts and keeping everything else
the same. Make a copy under `/tmp` so the example config stays unchanged:

```bash
cp examples/config.yaml /tmp/fgaperf-more-cohorts.yaml
perl -0pi -e 's/cohorts: 5/cohorts: 10/' /tmp/fgaperf-more-cohorts.yaml
./fgaperf all -config /tmp/fgaperf-more-cohorts.yaml -warmup 2s -duration 8s
```

Then compare two result JSON files:

```bash
./fgaperf compare results/results-A.json results/results-B.json
```

The comparison report names latency deltas and config differences so you can
tell whether the change was meaningful or apples-to-oranges.
When a p99 change looks close to noise, rerun each side with `-repeat 3` (or a
higher count) and compare the two result sets with `fgaperf compare a/*.json :
b/*.json` to get mean +/- stdev and significance labels.

## Next

Use [configuration-reference.md](configuration-reference.md) when you need the
full config schema, [recipes.md](recipes.md) for common measurement questions,
[methodology.md](methodology.md) for benchmarking caveats, and
[troubleshooting.md](troubleshooting.md) when a run stalls before producing a
useful report.
