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

## References

For full field definitions, see [configuration-reference.md](configuration-reference.md).
For measurement pitfalls behind these recipes, see [methodology.md](methodology.md).
