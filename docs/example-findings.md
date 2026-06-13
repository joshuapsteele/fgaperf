# OpenFGA Performance Test Findings

Generated 2026-06-13 01:52 UTC by fgaperf 0.1.0. All latencies in milliseconds, measured client-side over HTTP against http://localhost:8080.

New to these terms? Jump to the [How to read this](#how-to-read-this) section at the bottom for a per-column legend.

## Test configuration

*What this run looked like — the inputs that shape every number below. If you're comparing two runs, these are the rows that must match for the comparison to be apples-to-apples.*

| Parameter | Value |
|---|---|
| Endpoint | check |
| Consistency | MINIMIZE_LATENCY |
| Concurrency | 16 workers |
| Offered rate | closed loop |
| Warmup / measured | 3s / 15s (actual window 15.001s) |
| Seeded tuples | 2481 |
| Check corpus | 1000 entries (666 distinct checks) |
| Client | darwin/arm64, 10 CPU |

## Headline results

*Throughput and latency over the measured window. The Population column slices the same set of requests different ways: "All checks" is everything; the CEL/contextual rows split out paths that touched a CEL condition or carried request-scoped tuples. Compare populations of similar graph depth for a clean read.*

Sustained throughput was 5186 checks/sec over the 15.001s measured window, with 0 errors out of 77787 measured requests. All verified responses matched probe-time expectations.

| Population | Requests | Mean | p50 | p90 | p95 | p99 | Max |
|---|---|---|---|---|---|---|---|
| All checks | 77787 | 3.08 | 2.70 | 5.33 | 6.37 | 8.31 | 17.79 |
| CEL-conditioned paths | 31075 | 3.34 | 2.88 | 5.88 | 6.97 | 8.88 | 16.38 |
| Unconditioned paths | 46712 | 2.91 | 2.60 | 4.96 | 5.89 | 7.68 | 17.79 |
| With contextual tuples | 38340 | 3.04 | 2.67 | 5.23 | 6.27 | 8.24 | 15.06 |
| Without contextual tuples | 39447 | 3.13 | 2.74 | 5.41 | 6.47 | 8.37 | 17.79 |

Checks whose resolution path can evaluate a CEL condition ran 0.28 ms slower at p50 and 1.21 ms slower at p99 than checks on unconditioned relations. Note that conditioned and unconditioned populations also differ in graph depth, so this delta is an upper bound on pure CEL evaluation cost; compare relations of similar depth in the per-relation table below for a tighter read.

Checks carrying contextual tuples ran -0.07 ms slower at p50 and -0.13 ms slower at p99 than checks without contextual tuples. This split reflects the configured corpus mix; compare the same target relation with and without contextual assertions for the cleanest read.

## Per-relation breakdown

*Latency split out by relation. This is the cleanest place to ask "is one specific relation hot?" — populations above mix relations of different graph depth, but here every row is a single relation. A relation with much higher p99 than its peers usually means a deeper or denser resolution path; check the model.*

| Relation | Requests | Errors | Mean | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| document#can_share | 15365 | 0 | 2.52 | 2.30 | 4.74 | 6.13 |
| document#editor | 15464 | 0 | 3.04 | 2.78 | 5.68 | 7.25 |
| document#viewer | 15710 | 0 | 4.14 | 3.82 | 7.84 | 9.60 |
| folder#viewer | 15701 | 0 | 3.59 | 3.27 | 6.79 | 8.39 |
| group#member | 15547 | 0 | 2.11 | 1.89 | 4.13 | 5.56 |

## Server-side view

*OpenFGA's own metrics for the measured phase. The client-side numbers above include HTTP and JSON overhead; these don't. Use them to separate "the server is slow" from "the network/serialization is slow", and to size the datastore by datastore queries per request.*

Diffed from OpenFGA's Prometheus metrics between the start and end of the measured phase. Percentiles are estimated from histogram buckets, so they are coarser than the client-side numbers above.

| Metric | Value |
|---|---|
| Server-side request duration | mean 1.04 ms, p50 0.68, p95 4.43, p99 6.66 |
| Server-side requests observed | 77772 |
| Datastore queries per request | mean 9.60, p95 39, p99 48 |
| Total datastore queries | 746261 |
| Dispatches per request | mean 3.44, p95 17, p99 19 |

Datastore queries per request is the capacity currency for OpenFGA sizing: it tells you how much database load each check translates into, independent of network and JSON overhead.

## Write path

*A throwaway baseline for tuple writes. This is the bulk-seed path (large transactional batches), so it's faster than what a per-request Write would see. Treat it as a sanity check on the datastore's write headroom, not as a write-latency benchmark.*

Seeding 2481 tuples took 75.639458ms, a sustained write rate of 32800 tuples/sec using transactional Write calls.

## How to read this

Reference for the columns and terms used in this document. The README's Glossary covers the same terms with links to upstream OpenFGA documentation.

**Latency percentiles** — `p50` (median), `p90`, `p95`, `p99` are the latency values that fraction of requests came in under. `p99 = 8.0` means 99% of requests finished in 8 ms or less, but 1% (the tail) took longer. Tail latency drives user-visible pain; mean is rarely the right number to optimize.

**Service latency vs response latency** — service latency is measured from "request leaves the client" to "response arrives" — what `curl` would see. Response latency is measured from each request's *scheduled* send time, so when the server falls behind and requests queue waiting for a free worker, that queueing time shows up in response latency but not in service latency. Service latency understates pain under saturation; response latency is what your real callers feel.

**Offered rate vs achieved rate** — offered is what the load generator tried to send (set by `load.rate`). Achieved is what the server actually processed. Achieved < offered means the server fell behind; the gap shows up as dropped rate slots (a tick fired but every worker was still busy) and as rising response-latency p99.

**Throughput** — completed requests per second over the measured window. The Mismatches count is responses whose allowed/denied differs from probe-time ground truth — usually cache staleness, sometimes a real bug. The Errors count covers timeouts, 5xx, decode failures, etc.

**Population slices.** "All checks" is every measured Check or BatchCheck. "CEL-conditioned paths" are checks whose resolution can evaluate a CEL condition somewhere in the graph (computed statically from the model — fgaperf doesn't trace per request). "With contextual tuples" are checks where `contextual.attach_probability` won and the request carried contextual tuples. "Background tuple writes" is the churn rate's Write/Delete latency, only present when `load.write_rate > 0`.

**Per-relation table.** "Requests" is sample count for that relation in the measured window; "Errors" counts failures attributed to checks of that relation. Compare relations of similar graph depth — a deeper relation with higher latency may be entirely expected.

**Rate sweep.** "DS queries/req" is the server-reported mean datastore queries per Check at that offered rate; it rises sharply once OpenFGA starts spending most of its time on the database. The knee is the highest offered step that kept up (Achieved ≥ 98% of Offered) and, if `load.slo_p99` was set, also stayed under that SLO.

**Saturation knee** — the highest sustained rate. Past it, achieved rate plateaus and response-latency p99 climbs. Useful for capacity planning: the knee, minus headroom, is what you can safely send.

**Server-side view.** Diffed from OpenFGA's Prometheus histograms over the measured phase, so percentiles are bucket-estimated and slightly coarser than client-side. "Datastore queries per request" is the most portable capacity metric — independent of network and JSON overhead, so you can use it to size the database without identical client placement.

## Caveats and interpretation

The corpus replays 666 distinct checks across 1000 entries (1.5x average duplication). Duplication inflates server-side cache hit rates relative to production traffic; if it is high, lower probe.allowed_ratio pressure (or raise probe.samples_per_target) and rerun.

Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.

---

*This example was produced by `fgaperf all -config examples/config.yaml` (with a shortened 3s/15s warmup/measure window) against an OpenFGA server running the **Postgres-backed `docker-compose.yaml`** on the same laptop. The absolute numbers reflect that lightweight local setup, not a tuned production deployment — use your real deployment for numbers worth quoting. The document shows the report format: the inline section blurbs, the per-relation breakdown, the server-side view from Prometheus, and the "How to read this" legend at the bottom that defines every column.*
