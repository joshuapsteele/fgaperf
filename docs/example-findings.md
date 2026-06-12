# OpenFGA Performance Test Findings

Generated 2026-06-12 14:41 UTC by fgaperf 0.1.0. All latencies in milliseconds, measured client-side over HTTP against http://localhost:8080.

## Test configuration

| Parameter | Value |
|---|---|
| Endpoint | check |
| Consistency | MINIMIZE_LATENCY |
| Concurrency | 16 workers |
| Offered rate | closed loop |
| Warmup / measured | 3s / 15s |
| Seeded tuples | 2481 |
| Check corpus | 1000 entries |
| Client | darwin/arm64, 10 CPU |

## Headline results

Sustained throughput was 15604 checks/sec with 0 errors out of 234059 measured requests. All verified responses matched probe-time expectations.

| Population | Requests | Mean | p50 | p90 | p95 | p99 | Max |
|---|---|---|---|---|---|---|---|
| All checks | 234059 | 1.02 | 0.90 | 1.76 | 2.17 | 2.92 | 10.01 |
| CEL-conditioned paths | 93795 | 1.16 | 1.02 | 1.99 | 2.41 | 3.17 | 10.01 |
| Unconditioned paths | 140264 | 0.94 | 0.82 | 1.58 | 1.95 | 2.68 | 7.24 |

Checks whose resolution path can evaluate a CEL condition ran 0.19 ms slower at p50 and 0.49 ms slower at p99 than checks on unconditioned relations. Note that conditioned and unconditioned populations also differ in graph depth, so this delta is an upper bound on pure CEL evaluation cost; compare relations of similar depth in the per-relation table below for a tighter read.

## Per-relation breakdown

| Relation | Requests | Mean | p50 | p95 | p99 |
|---|---|---|---|---|---|
| document#can_share | 47027 | 0.95 | 0.85 | 1.94 | 2.61 |
| document#editor | 46432 | 1.00 | 0.89 | 2.00 | 2.67 |
| document#viewer | 46768 | 1.36 | 1.20 | 2.69 | 3.41 |
| folder#viewer | 46885 | 1.13 | 1.00 | 2.26 | 2.92 |
| group#member | 46947 | 0.68 | 0.60 | 1.37 | 1.88 |

## Write path

Seeding 2481 tuples took 36.535209ms, a sustained write rate of 67907 tuples/sec using transactional Write calls.

## Caveats and interpretation

Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.

---

*This example was produced by `fgaperf all -config examples/config.yaml` (with a shortened 3s/15s warmup/measure window) against an OpenFGA server running the **in-memory** datastore on the same laptop. The absolute numbers are therefore meaningless as a benchmark — use the Postgres-backed `docker-compose.yaml`, or your real deployment, for numbers worth quoting. The document shows the report format: the CEL-conditioned vs unconditioned split, the per-relation breakdown, and the verification that responses under load matched probe-time expectations.*
