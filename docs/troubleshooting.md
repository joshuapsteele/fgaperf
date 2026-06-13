# Troubleshooting

This page covers the failure modes that usually happen before a run produces a
useful findings document. Start with `fgaperf doctor -config config.yaml`; it
checks the same basics and prints command-specific hints.

## OpenFGA Not Reachable

**Problem:** commands fail with `connection refused`, `i/o timeout`, or
`no such host`.

**Diagnosis:** confirm the URL and port from your config:

```bash
./fgaperf validate -config config.yaml
docker compose ps
curl -sf http://localhost:8080/healthz
```

**Fix:** start the bundled stack with `docker compose up -d`, or update
`openfga.api_url` to the server you actually want to test. If you use Colima
or another Docker context, `docker info` should succeed before compose will.
If fgaperf runs inside a container and OpenFGA runs on the host, use
`http://host.docker.internal:8080` when your runtime supports it.

## Store Not Found Mid-Run

**Problem:** `probe`, `run`, or `cleanup` reports a 404 for a store ID.

**Diagnosis:** `.fgaperf-state.json` points at the store created by `setup`.
If that store was deleted manually, by `all`, or by another cleanup pass, the
state file is stale.

**Fix:** run a fresh workflow:

```bash
./fgaperf cleanup -config config.yaml -all-stores
./fgaperf setup -config config.yaml
./fgaperf probe -config config.yaml
./fgaperf run -config config.yaml
```

Use `./fgaperf all -config config.yaml` when you do not need to keep the store.
Use `-keep` or `keep_store: true` when you want to reuse a seeded store.

## Probe Corpus Is Empty Or All-Denied

**Problem:** `probe` writes few entries, warns about only denied outcomes, or
`run` says the corpus is empty.

**Diagnosis:** inspect the target relation and graph shape:

```bash
./fgaperf inspect -config config.yaml
./fgaperf plan -config config.yaml
```

Common causes are a target relation with no reachable direct tuple path, a
model/config mismatch, too few instances, or `probe.cohort_bias` so low that
intersection and tuple-to-userset paths rarely meet inside the same cohort.

**Fix:** raise `probe.samples_per_target`, raise `probe.cohort_bias`, target a
relation with assignable inputs, or use `probe.allowed_ratio: -1` to keep the
natural allowed/denied mix. See the `probe.*` fields in
[configuration-reference.md](configuration-reference.md).

## Verification Mismatches Under Churn

**Problem:** the findings report shows `result_mismatches > 0`, especially
with `load.write_rate` enabled.

**Diagnosis:** fgaperf probes ground truth with `HIGHER_CONSISTENCY`, then load
may run with `MINIMIZE_LATENCY`. Under write churn, cached reads can
temporarily disagree with freshly probed answers.

**Fix:** decide whether stale reads are acceptable for the caller you are
modeling. For freshness-sensitive paths, rerun with:

```yaml
load:
  consistency: HIGHER_CONSISTENCY
```

If mismatches remain with `HIGHER_CONSISTENCY` and no write churn, inspect the
mismatch JSON file named in the findings report.

## Numbers Do Not Match Production

**Problem:** the run is stable, but latency or throughput does not resemble
production.

**Diagnosis:** check what differs: datastore type, cache settings, client
placement, request endpoint, result-set size, warmup length, corpus uniqueness,
and write traffic. The findings Summary, Server-side view, Caveats, and
resolved config are the first places to look.

**Fix:** point `metrics.prometheus_url` at OpenFGA metrics, use the same
datastore class as production, place the client where production callers live,
extend `load.warmup`, add `load.write_rate` when production writes are steady,
and use weighted `probe.targets` when production traffic is skewed. The
methodology notes in [methodology.md](methodology.md) explain why these
differences dominate benchmark results.
