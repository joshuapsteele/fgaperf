# OpenFGA Performance Test Findings

Generated 2026-01-02 15:04 UTC by fgaperf 0.1.0. All latencies in milliseconds, measured client-side over HTTP against http://localhost:8080.

New to these terms? Jump to the [How to read this](#how-to-read-this) section at the bottom for a per-column legend.

## Test configuration

*What this run looked like — the inputs that shape every number below. If you're comparing two runs, these are the rows that must match for the comparison to be apples-to-apples.*

| Parameter | Value |
|---|---|
| Endpoint | check |
| Consistency | MINIMIZE_LATENCY |
| Concurrency | 16 workers |
| Offered rate | closed loop |
| Warmup / measured | 10s / 1m0s (actual window 1m0.012s) |
| Seeded tuples | 2481 |
| Check corpus | 1000 entries (666 distinct checks) |
| Client | linux/amd64, 8 CPU |

## Summary

Sustained 4892 checks/sec over 1m0.012s. Client-side p99 was 8.00 ms. OpenFGA reported 9.70 datastore queries/request. Verified responses had zero mismatches.

## Headline results

*Throughput and latency over the measured window. The Population column slices the same set of requests different ways: "All checks" is everything; the CEL/contextual rows split out paths that touched a CEL condition or carried request-scoped tuples. Compare populations of similar graph depth for a clean read.*

Sustained throughput was 4892 checks/sec over the 1m0.012s measured window, with 0 errors out of 294000 measured requests. All verified responses matched probe-time expectations.

| Population | Requests | Mean | p50 | p90 | p95 | p99 | Max |
|---|---|---|---|---|---|---|---|
| All checks | 294000 | 3.00 | 2.00 | 5.00 | 6.00 | 8.00 | 40.00 |
| CEL-conditioned paths | 118000 | 4.00 | 3.00 | 6.00 | 7.00 | 10.00 | 38.00 |
| Unconditioned paths | 176000 | 2.00 | 2.00 | 4.00 | 5.00 | 7.00 | 40.00 |
| With contextual tuples | 145000 | 3.00 | 3.00 | 6.00 | 7.00 | 9.00 | 30.00 |
| Without contextual tuples | 149000 | 2.00 | 2.00 | 4.00 | 5.00 | 7.00 | 40.00 |

Checks whose resolution path can evaluate a CEL condition ran 1.00 ms slower at p50 and 3.00 ms slower at p99 than checks on unconditioned relations. Note that conditioned and unconditioned populations also differ in graph depth, so this delta is an upper bound on pure CEL evaluation cost; compare relations of similar depth in the per-relation table below for a tighter read.

Checks carrying contextual tuples ran 1.00 ms slower at p50 and 2.00 ms slower at p99 than checks without contextual tuples. This split reflects the configured corpus mix; compare the same target relation with and without contextual assertions for the cleanest read.

## Latency over time

*The measured window sliced into time buckets. Aggregate percentiles hide *when* latency was bad; this catches cache fill-in (early buckets slow, later ones fast), GC pauses or compaction (one bucket spikes), and gradual degradation (p99 trending up). The bar tracks p99 relative to the worst bucket.*

| Time | Requests | Throughput/s | p50 | p99 | p99 trend | Errors |
|---|---|---|---|---|---|---|
| t+0s | 4195 | 4195 | 3.00 | 16.00 | ████████████████████ | 1 |
| t+5s | 24700 | 4940 | 2.00 | 8.00 | ██████████ | 0 |
| t+10s | 24640 | 4928 | 2.00 | 8.00 | ██████████ | 2 |

## Per-relation breakdown

*Latency split out by relation. This is the cleanest place to ask "is one specific relation hot?" — populations above mix relations of different graph depth, but here every row is a single relation. A relation with much higher p99 than its peers usually means a deeper or denser resolution path; check the model.*

| Relation | Requests | Errors | Mean | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| document#editor | 98000 | 0 | 2.00 | 2.00 | 5.00 | 7.00 |
| document#viewer | 196000 | 0 | 3.00 | 3.00 | 7.00 | 9.00 |

## Errors

*Failed requests grouped by class. Timeouts and 5xx point at server- or datastore-side trouble (look at the server-side view, or lower offered rate). 4xx and decode errors point at fgaperf or config (mismatched model, malformed contextual tuples). Connection errors usually mean the server restarted mid-run. The `batch-item` class counts batch-check calls whose HTTP round trip succeeded but that carried at least one item-level error; their service latency is still included in the percentiles above, since the request itself completed.*

| Class | Count |
|---|---|
| 5xx | 1 |
| timeout | 2 |

First error messages observed:

- `POST /stores/.../check: HTTP 500: internal error`

## Server-side view

*OpenFGA's own metrics for the measured phase. The client-side numbers above include HTTP and JSON overhead; these don't. Use them to separate "the server is slow" from "the network/serialization is slow", and to size the datastore by datastore queries per request. On a shared OpenFGA deployment, these Prometheus counters may include unrelated traffic unless the server exposes labels you can isolate.*

Diffed from OpenFGA's Prometheus metrics between the start and end of the measured phase. Percentiles are estimated from histogram buckets, so they are coarser than the client-side numbers above.

| Metric | Value |
|---|---|
| Server-side request duration | mean 2.10 ms, p50 1.80, p95 5.10, p99 7.40 |
| Server-side requests observed | 294000 |
| Datastore queries per request | mean 9.70, p95 18, p99 24 |
| Total datastore queries | 2851800 |
| Check cache hit rate | 40.8% of 294000 lookups |

Datastore queries per request is the capacity currency for OpenFGA sizing: it tells you how much database load each check translates into, independent of network and JSON overhead.

## Write path

*A throwaway baseline for tuple writes. This is the bulk-seed path (large transactional batches), so it's faster than what a per-request Write would see. Treat it as a sanity check on the datastore's write headroom, not as a write-latency benchmark.*

Seeding 2481 tuples took 97ms, a sustained write rate of 25577 tuples/sec using transactional Write calls.

## How to read this

Reference for the columns and terms used in this document. The README's Glossary covers the same terms with links to upstream OpenFGA documentation.

**Latency percentiles** — `p50` (median), `p90`, `p95`, `p99` are the latency values that fraction of requests came in under. `p99 = 8.0` means 99% of requests finished in 8 ms or less, but 1% (the tail) took longer. Tail latency drives user-visible pain; mean is rarely the right number to optimize.

**Service latency vs response latency** — service latency is measured from "request leaves the client" to "response arrives" — what `curl` would see. Response latency is measured from each request's *scheduled* send time, so when the server falls behind and requests queue waiting for a free worker, that queueing time shows up in response latency but not in service latency. Service latency understates pain under saturation; response latency is what your real callers feel.

**Offered rate vs achieved rate** — offered is what the load generator tried to send (set by `load.rate`). Achieved is what the server actually processed. Achieved < offered means the server fell behind; the gap shows up as dropped rate slots (a tick fired but every worker was still busy) and as rising response-latency p99.

**Throughput** — completed requests per second over the measured window. The Mismatches count is responses whose allowed/denied differs from probe-time ground truth — usually cache staleness, sometimes a real bug. The Errors count covers timeouts, 5xx, decode failures, etc.

**Population slices.** "All checks" is every measured Check or BatchCheck. "CEL-conditioned paths" are checks whose resolution can evaluate a CEL condition somewhere in the graph (computed statically from the model — fgaperf doesn't trace per request). "With contextual tuples" are checks where `contextual.attach_probability` won and the request carried contextual tuples. "Background tuple writes" is the churn rate's Write/Delete latency, only present when `load.write_rate > 0`.

**Per-relation table.** "Requests" is sample count for that relation in the measured window; "Errors" counts failures attributed to checks of that relation. Compare relations of similar graph depth — a deeper relation with higher latency may be entirely expected. The "DS queries/check (probe)" column appears only when `probe.attribute_ds_queries` ran with a metrics endpoint: it reports how many datastore reads each relation's check costs, attributed one relation at a time at probe time — the sharpest signal for spotting an expensive rewrite.

**Latency over time.** The measured window split into equal time buckets (the width adapts so any run is ~12 rows). "Throughput/s" divides each bucket's completed items by the bucket width. Read it as a trend: early buckets slower than later ones is cache warming; a single bucket spiking is a GC pause or datastore compaction; p99 trending upward across buckets is the server falling behind. The last bucket may be partial and read low.

**Rate sweep.** "DS queries/req" is the server-reported mean datastore queries per Check at that offered rate; it rises sharply once OpenFGA starts spending most of its time on the database. The knee is the highest offered step that kept up (Achieved ≥ 98% of Offered) and, if `load.slo_p99` was set, also stayed under that SLO.

**Saturation knee** — the highest sustained rate. Past it, achieved rate plateaus and response-latency p99 climbs. Useful for capacity planning: the knee, minus headroom, is what you can safely send.

**Server-side view.** Diffed from OpenFGA's Prometheus histograms over the measured phase, so percentiles are bucket-estimated and slightly coarser than client-side. "Datastore queries per request" is the most portable capacity metric — independent of network and JSON overhead, so you can use it to size the database without identical client placement. On shared servers, confirm the scraped metrics are not mixed with unrelated traffic.

## Caveats and interpretation

The corpus replays 666 distinct checks across 1000 entries (1.5x average duplication). Duplication inflates server-side cache hit rates relative to production traffic; if it is high, lower probe.allowed_ratio pressure (or raise probe.samples_per_target) and rerun.

Latencies include client-side HTTP and JSON overhead, which is the number a calling service would actually observe. Results depend heavily on the datastore behind OpenFGA, its cache configuration, and co-location of client and server; record those alongside these numbers. The conditioned/unconditioned split is computed statically from the model (whether any tuple on the resolution path can carry a condition), not from per-request traces. Repeat runs with different random_seed values to confirm stability before drawing conclusions.

For the measurement pitfalls behind these caveats — closed-loop vs fixed-rate, coordinated omission, warmup and cache fill-in, corpus uniqueness, and why probing and load can legitimately disagree — see the [benchmarking methodology](https://github.com/joshuapsteele/fgaperf/blob/main/docs/methodology.md) page.
