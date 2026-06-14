# Configuration Recipes

Each recipe starts from `examples/config.yaml`. Copy it to `/tmp` or your own
config file, apply the YAML snippet, then run the command shown. The snippets
use the bundled compose stack and example model.

## Find Max Sustained Throughput

```yaml
load:
  rate: 0
  sweep:
    rates: [200, 500, 1000, 2000, 4000]
    step_duration: 30s
  slo_p99: 25ms
```

Run:

```bash
./fgaperf all -config config.yaml
```

The findings Summary and Rate sweep table mark the saturation knee: the
highest offered rate that kept up and stayed under the optional p99 SLO.

## Measure Cache Impact

Run the same seeded store twice, changing only consistency:

```yaml
load:
  consistency: MINIMIZE_LATENCY
```

Then:

```yaml
load:
  consistency: HIGHER_CONSISTENCY
```

Use `setup -keep`, `probe`, then two `run` commands against the same corpus, or
run two full `all` tests with the same `random_seed`. Compare the results with
`fgaperf compare`.

## Compare Two Model Versions

```yaml
model_file: examples/model.json
random_seed: 42
```

Run once for the baseline, then change only `model_file` to the new compiled
model JSON and run again. Keep `seed`, `probe`, and `load` identical so the
comparison report can isolate model behavior instead of workload drift.

## Test Under Write Churn

```yaml
load:
  write_rate: 50
  consistency: MINIMIZE_LATENCY
```

This adds background Write/Delete traffic during the measured phase. The
findings report includes a Background tuple writes row and explains any
verification mismatches in the context of cached reads.

## Spot a Hot Relation

```yaml
probe:
  targets:
    - {relation: document#viewer, weight: 10}
    - {relation: document#editor, weight: 1}
    - {relation: group#member, weight: 1}
```

Probe still samples each target evenly, but the load phase sends more traffic
to the weighted relation. Use this when production traffic is concentrated on
one permission path.

To find *why* a relation is hot — is it doing far more datastore work than its
peers? — turn on per-relation datastore-query attribution (needs a metrics
endpoint):

```yaml
metrics:
  prometheus_url: http://localhost:2112
probe:
  attribute_ds_queries: true
```

The findings per-relation table then gains a "DS queries/check (probe)" column.
A relation reading many datastore rows per check (a deep tuple-to-userset path)
next to one reading a couple (a direct relation) tells you where to look in the
model. Best-effort; run it against a dedicated server for clean numbers.

## Replay Production Traffic

```yaml
corpus_source: replay
replay:
  file: production-checks.jsonl
```

When you have a real check log — an OpenFGA request log or app audit trail —
replay reproduces *that* distribution instead of synthesizing one, so the load
mix matches production exactly. The log is JSONL, one check per line:

```jsonl
{"user":"user:1","relation":"viewer","object":"document:1"}
{"user":"user:2","relation":"editor","object":"document:9","context":{"scope":"write"}}
```

Extra fields (store IDs, timestamps, ...) are ignored, so a raw request log
works as-is. fgaperf learns each distinct entry's ground truth the same way the
probe does, then weights the load by the log's per-target frequencies. Seed the
store from the same model the log was captured against, so the log's
`user`/`object` IDs resolve. Malformed lines are reported and skipped.

## Reproduce a Noisy Run

```yaml
random_seed: 42
load:
  sample_file: samples.jsonl.gz
```

The same model, config, and `random_seed` regenerate the same tuple graph and
probe corpus. `sample_file` records raw measured samples for offline analysis.

## Run Before and After a Server Upgrade

```yaml
metrics:
  prometheus_url: http://localhost:2112
load:
  sweep:
    rates: [200, 500, 1000, 2000]
    step_duration: 30s
```

Run against the old server, upgrade OpenFGA or the datastore, run again, then:

```bash
./fgaperf compare results/results-before.json results/results-after.json
```

The comparison includes client latency, server-side duration, datastore queries
per request, and config differences.

## Gate CI on a Performance Regression

Capture a baseline from a representative run, then fail the build when a later
run regresses past a threshold:

```bash
./fgaperf all -config examples/config.yaml -output-dir results
./fgaperf baseline save -output-dir results "$(ls -t results/results-*.json | head -1)"
cp "$(ls -t results/baseline-*.json | head -1)" baseline.json   # commit this

# later — on every PR:
./fgaperf all -config examples/config.yaml -output-dir results
./fgaperf compare -against-baseline baseline.json \
  -max-regression "p99=10%,throughput=-5%" \
  "$(ls -t results/results-*.json | head -1)"
```

`compare -against-baseline` exits non-zero (naming the metric and target) when
overall or any per-target p99 rises more than 10%, or throughput drops more than
5%. Add `-exit-on-regression=false` for an advisory, non-blocking comparison.
The full GitHub Actions recipe, threshold reference, and baseline-refresh
workflow are in [ci-regression-gate.md](ci-regression-gate.md).

## References

For full field definitions, see [configuration-reference.md](configuration-reference.md).
For measurement pitfalls behind these recipes, see [methodology.md](methodology.md).
