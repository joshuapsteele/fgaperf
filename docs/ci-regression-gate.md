# CI Performance-Regression Gate

Unit tests catch correctness regressions; this gate catches *performance*
regressions — an OpenFGA upgrade, a datastore tuning change, or a model edit
that quietly makes checks slower or more expensive. The mechanism is two
commands built on the results JSON every run already writes:

1. `fgaperf baseline save <results.json>` — distills a run into a compact,
   long-lived `baseline-<stamp>.json` (run shape, config fingerprint, random
   seed, throughput, key latency percentiles, per-target p99, and server-side
   datastore cost when present).
2. `fgaperf compare -against-baseline <baseline.json> <results.json>` — compares
   a fresh run to that baseline and **exits non-zero** when any tracked metric
   regresses past its threshold, naming the metric and target. That non-zero
   exit is what fails the CI job.

## Thresholds

`-max-regression` is a comma-separated list of `metric=percent` limits. The
default is `p99=10%,throughput=-5%` — fail if overall **or any per-target** p99
rises more than 10%, or if throughput drops more than 5%.

| Metric | Direction | Meaning of the percent |
|---|---|---|
| `mean`, `p50`, `p90`, `p95`, `p99`, `max` | latency, lower is better | max allowed **increase** (overall and per-target) |
| `throughput` | higher is better | max allowed **decrease** (write it as a negative, e.g. `-5%`) |
| `ds_queries` | lower is better | max allowed increase in server datastore queries/request |
| `server_p99` | lower is better | max allowed increase in the server-side request p99 |

`ds_queries` and `server_p99` only fire when both the baseline and the current
run carry server-side metrics (`metrics.prometheus_url` configured). A gate that
can't be evaluated (a missing metric, a target absent from the current run, a
zero baseline) is downgraded to a warning, never a silent pass.

Sign is normalized for you: thresholds are always read as "how far in the
*worse* direction is allowed," so `throughput=5%` and `throughput=-5%` mean the
same thing, and a latency `p99=-10%` is treated as `p99=10%`.

## Advisory mode

Pass `-exit-on-regression=false` to report regressions **without** failing the
job — the comparison markdown and the per-breach warnings are still written, but
the command exits zero. Use it for a non-blocking PR check or a trend dashboard
while you tune thresholds, then drop the flag (the gate is on by default) once
you trust them.

## Refreshing the baseline

A baseline is only meaningful against the same workload. Regenerate it whenever
you *intend* the numbers to move (a deliberate model change, a new server target
you're now standardizing on), and commit the new `baseline.json`:

```bash
./fgaperf all -config examples/config.yaml -output-dir results
./fgaperf baseline save -output-dir results "$(ls -t results/results-*.json | head -1)"
cp "$(ls -t results/baseline-*.json | head -1)" baseline.json
git add baseline.json && git commit -m "Refresh perf baseline"
```

Keep the config that produced it pinned alongside (same `random_seed`,
`model_file`, and `seed.*`/`load.*` block). `compare` warns when the resolved
config fingerprint drifts, so an accidental workload change shows up as a caveat
rather than a misleading pass or fail.

## GitHub Actions recipe

This job checks the committed `baseline.json` into the gate, runs the full
pipeline against a throwaway OpenFGA, and fails the build on a regression. The
in-memory datastore is fine for a functional gate; point it at a Postgres-backed
stack (see `docker-compose.yaml`) when you want production-shaped latency.

```yaml
name: perf-gate

on:
  pull_request:
  push:
    branches: [main]

jobs:
  perf-gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go build -o fgaperf .

      - name: Start OpenFGA
        run: |
          docker run -d --name openfga -p 8080:8080 openfga/openfga:latest run
          for i in $(seq 1 30); do
            curl -sf http://localhost:8080/healthz > /dev/null && exit 0
            sleep 1
          done
          echo "OpenFGA did not become healthy" >&2
          docker logs openfga >&2
          exit 1

      - name: Run the load test
        run: |
          ./fgaperf all -config examples/config.yaml -output-dir results
          echo "RESULTS=$(ls -t results/results-*.json | head -1)" >> "$GITHUB_ENV"

      - name: Gate against the committed baseline
        run: |
          ./fgaperf compare -against-baseline baseline.json \
            -max-regression "p99=10%,throughput=-5%" \
            -output-dir results "$RESULTS"

      # Always upload the comparison + findings so a failed gate is debuggable.
      - name: Upload reports
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: fgaperf-reports
          path: |
            results/baseline-compare-*.md
            results/findings-*.md
            results/results-*.json
```

A few practical notes:

- **Pin the runner shape.** Latency on a shared CI runner is noisier than on a
  dedicated host; keep the thresholds wide enough to absorb that noise (the
  default `p99=10%` is a reasonable floor on GitHub-hosted runners) or run the
  gate on a self-hosted runner for tighter bounds.
- **Find the results file by glob.** Results filenames are timestamped, so
  `ls -t results/results-*.json | head -1` is the portable way to grab the run
  you just produced.
- **Compose stack for realism.** Swap the single `docker run` for
  `docker compose up -d` against the bundled Postgres stack when client-side
  numbers from the in-memory store are too optimistic to gate on.

## Self-test in this repo

`.github/workflows/ci.yaml` has a `perf-gate` job that exercises this end to end
on every push: it runs the pipeline, saves a baseline, confirms the gate
**passes** the healthy run, then forges an unbeatable baseline (1 ns p99) and
confirms the gate **fails** the now-regressed run. That is the executable
version of the acceptance test for this feature.
