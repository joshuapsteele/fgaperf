# Benchmarking methodology

A performance number is only as trustworthy as the method that produced it.
This page collects the measurement pitfalls fgaperf is built to avoid — and the
ones it can't avoid for you — so you can read its output critically and design
runs that answer the question you actually have.

If you just want to get a run going, start with the
[README Quick Start](../README.md#quick-start); come back here when a number
surprises you.

## Closed-loop vs fixed-rate: each lies to you differently

There are two ways to drive load, and they answer different questions.

**Closed-loop** (`load.rate: 0`) issues the next request the moment the previous
one returns. With *N* workers there are never more than *N* requests in flight,
so the system is self-throttling: if the server slows down, the load generator
automatically backs off. This is the right tool for finding **maximum
throughput** — push concurrency up until throughput plateaus. But it
*systematically understates latency under contention*: because the generator
slows down exactly when the server does, it never builds the queue a real
fleet of independent callers would. The latency you measure is the latency of
a client that politely waits its turn.

**Fixed-rate** (`load.rate: 2000`) sends requests on a schedule regardless of
whether the server is keeping up. This models real production traffic, where
callers arrive on their own clock and don't care that the server is busy. It is
the right tool for **latency at a target load**. Its failure mode is the
opposite: if you set the rate above what the server can sustain, the run
saturates and the numbers describe an overloaded system, which may not be what
you meant to measure.

The honest answer to "how fast is this deployment?" usually needs both: a
closed-loop run to find the ceiling, then a `load.sweep` (below) to map latency
as a function of offered rate up to that ceiling.

## Coordinated omission

This is the subtle one, and it's why fixed-rate runs report two latency numbers.

Naive load generators measure latency from *when the request was sent* to *when
the response arrived*. But if the server stalls, a naive generator stalls with
it — it doesn't send the next request until the current one returns. The
requests that *would have been sent during the stall* are simply never issued.
The slowest moments of the run are the ones with the fewest samples. The result
is a latency distribution that looks great precisely because the bad periods are
under-represented. This is **coordinated omission**: the measurement omits the
samples that coordinate with the server's bad behavior.

fgaperf corrects for this in fixed-rate mode by measuring **response latency**
from each request's *scheduled* send time, not its actual send time. If slot
*N* was supposed to fire at `start + N×interval` but every worker was busy until
later, the wait counts against that request. The findings doc shows both:

- **Service latency** — request sent to response received. What `curl` sees.
  Understates pain under saturation.
- **Response latency** — scheduled send to response received. Includes the
  queueing delay a saturated server imposes. What your real callers feel.

When the two diverge, the server is not keeping up and the response-latency row
is the one to trust. The achieved-rate-vs-offered-rate line and the dropped-slot
count tell you the same story from the throughput side.

Closed-loop runs have no schedule to fall behind, so they report only service
latency — but remember they sidestep coordinated omission by sidestepping the
queue entirely, which is its own bias.

## Warmup and cache fill-in

A cold server is not the server you run in production. Connection pools are
empty, the datastore's buffer cache is cold, and (if enabled) OpenFGA's check
query cache holds nothing. The first requests of a run pay for all of that, and
including them drags the percentiles toward a state the server is never in
during steady operation.

`load.warmup` runs the load for a configured period whose samples are discarded
before the measured window opens. The **Latency over time** section of the
findings doc lets you confirm warmup was long enough: if the first measured
bucket is markedly slower than the rest, the cache was still filling when
measurement began — raise `load.warmup` and rerun. (The example findings show
exactly this effect when warmup is too short: an early p99 spike that settles
within a second or two.)

There is a tension here. Warmup makes the steady-state numbers clean, but a real
production deployment *also* serves cold-cache requests after every deploy,
failover, or cache eviction. Warmup measures best-case steady state on purpose;
if cold-start latency matters to you, measure it deliberately with a short or
zero warmup and read the first timeline buckets.

## Corpus uniqueness vs the query cache

fgaperf learns its check corpus empirically (it executes candidate checks once
under `HIGHER_CONSISTENCY` to record ground truth), then replays that corpus
under load. If the corpus contains only a handful of *distinct* checks repeated
many times, and OpenFGA's check query cache is enabled, the load phase becomes a
cache-hit benchmark: you measure how fast OpenFGA returns a cached answer, not
how fast it evaluates the model.

This matters most when the natural allowed/denied mix is lopsided. To hit
`probe.allowed_ratio`, the prober resamples with replacement; when allowed (or
denied) outcomes are scarce, that means duplicating a few entries many times.
fgaperf bounds this with `probe.max_duplication` and reports
distinct-vs-total corpus entries in both the probe output and the findings doc's
caveats. If duplication is high, either raise `probe.samples_per_target` (more
candidates to draw from) or set `probe.allowed_ratio: -1` to keep the natural
mix instead of forcing a target ratio.

Production traffic has its own cache-hit rate, of course — the goal isn't zero
duplication, it's *duplication that resembles production*. But a corpus of 5
distinct checks replayed 200 times resembles nothing.

## Why `HIGHER_CONSISTENCY` probing and `MINIMIZE_LATENCY` load can legitimately disagree

By default fgaperf probes ground truth under `HIGHER_CONSISTENCY` (which bypasses
caches for a definitive answer) and replays under `MINIMIZE_LATENCY` (the fast,
cached path — the production default). With `load.verify_results: true`, the
load phase compares each cached response against the probe-time ground truth and
counts **mismatches**.

A nonzero mismatch count is not automatically a bug. With background churn
(`load.write_rate > 0`), tuples change during the run; a `MINIMIZE_LATENCY`
check may legitimately return a slightly stale answer that differs from the
ground truth recorded earlier. That is the cache doing its job — trading
freshness for latency. The mismatch count quantifies that staleness, which is
exactly the tradeoff you'd want to measure before choosing a consistency mode.

It *is* worth investigating when mismatches appear **without** churn and a
static store, or when the rate is far higher than cache invalidation can
explain. The `mismatches-<stamp>.json` file lists the specific checks
(deduplicated) so you can reproduce them by hand. As a rule: if fresh reads
matter to your callers, measure under `HIGHER_CONSISTENCY` and accept the higher
latency; if you run `MINIMIZE_LATENCY` in production, measure it and let the
mismatch count tell you how stale the cache gets.

## Client and server co-location

fgaperf measures **client-side** latency over HTTP: network round-trip, JSON
serialization, and OpenFGA's own work, all rolled together. Where you run the
load generator relative to the server changes the number materially. A generator
on the same host as OpenFGA measures almost-pure server work; one across a
region measures mostly network.

Neither is wrong, but they answer different questions, and you must record which
you did. Two levers help separate the components:

- The **server-side view** (set `metrics.prometheus_url`) reports OpenFGA's own
  request-duration histogram and, crucially, **datastore queries per request** —
  the most portable capacity metric, independent of where your client sits. When
  client-side p99 is far above server-side p99, the gap is network and
  serialization, not OpenFGA.
- Running the generator close to the server isolates server + datastore behavior;
  running it where your real callers sit measures end-to-end caller experience.

For publishable numbers, run fgaperf near the OpenFGA and datastore deployment
you actually care about, and report client placement, datastore engine, and
cache configuration alongside the latencies. A local Docker stack is excellent
for relative comparisons and smoke tests, and misleading as an absolute number.

## Change one variable per run

The tool's natural use is comparative: consistency on vs off, cache on vs off,
two model versions, two OpenFGA releases, two instance counts. The only way a
comparison means anything is if exactly one thing changed between the two runs.

fgaperf supports this discipline directly:

- Results JSON embeds the full **resolved config** (post-defaults, credentials
  redacted) and the environment, so a results file is a complete record of how
  it was produced.
- `random_seed` makes generation deterministic: the same seed and config
  regenerate the same tuple graph and corpus, so a rerun differs only in the
  variable you changed, not in the random world.
- `fgaperf compare a.json b.json` renders two results side by side, computes
  per-relation deltas, and **names exactly which config keys differed** — so if
  you accidentally changed two things, the comparison tells you.

When in doubt, fix `random_seed`, change one knob, rerun, and `compare`. A
delta you can't attribute to a single named difference is not a result.

## See also

- [Configuration reference](configuration-reference.md) — every knob, with
  defaults and notes.
- [Example findings](example-findings.md) — an annotated run.
- The findings doc's own "How to read this" and "Caveats" sections — generated
  per run, with the specific numbers filled in.
