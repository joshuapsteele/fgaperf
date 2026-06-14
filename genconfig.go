package main

// genconfig.go renders a starter config.yaml from a compiled model. Every
// section that examples/config.yaml uses is emitted with the same teaching
// comments, but values are picked from the model's shape:
//
//   - instances: subject-only types ("user-like", no relations of their own)
//     get a larger pool than container types.
//   - fanout: relations whose accepted user types include a userset (e.g.
//     group#member) get a higher per-user-type fanout — that pattern is the
//     usual group-membership shape, where fanout 2 means almost no allowed
//     paths resolve. Self-referential relations are pinned to fanout 1 so
//     the generator's "edges only point downward" invariant doesn't get
//     thrown off by a generous default.
//   - wildcard_probability: 0 if the model has no wildcard refs anywhere,
//     0.5 otherwise.
//   - contextual.relations: any relation whose name suggests a per-request
//     fact (context|session|active|current). Emitted commented out when no
//     match exists, so the user sees the knob.
//   - conditions/pools: one block per CEL condition in the model, with the
//     tuple/request split following the same map+list → tuple heuristic that
//     model.go's TupleContextParams uses, and one pool per param.
//   - probe.targets: every (type, relation) in the model, alphabetized.
//
// The output is annotated YAML, not yaml.Marshal output — the comments matter
// more than the lines.

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

var contextualNameRE = regexp.MustCompile(`(?i)(context|session|active|current)`)

// generateConfig writes an annotated config.yaml starting point to w. The
// model_file path is echoed verbatim (we don't second-guess what the user
// typed); all other fields are derived from the parsed model.
func generateConfig(modelPath string, a *Analysis, w io.Writer) error {
	var b strings.Builder

	hasWildcards := analysisHasWildcards(a)
	hasConditions := len(a.Model.Conditions) > 0
	contextualGuesses := guessContextualRelations(a)
	instances := pickInstances(a)
	fanout := pickFanout(a)

	writeHeader(&b, modelPath)
	writeOpenFGA(&b)
	writeTopLevel(&b, modelPath)
	writeSeed(&b, a, instances, fanout, hasWildcards, contextualGuesses)
	writeContextual(&b, contextualGuesses)
	if hasConditions {
		writeConditions(&b, a)
		writePools(&b, a)
	} else {
		writeNoConditionsPools(&b)
	}
	writeProbe(&b, a)
	writeLoad(&b)
	writeMetrics(&b)

	_, err := io.WriteString(w, b.String())
	return err
}

func writeHeader(b *strings.Builder, modelPath string) {
	fmt.Fprintf(b, `# fgaperf configuration, generated from %s by `+"`fgaperf gen-config`."+`
# Every field is optional; built-in defaults produce a working run for any
# compiled model. Durations use Go syntax (30s, 5m).
#
# This file is a starting point — tune cohorts/instances/fanout to match the
# scale of the system you're modeling, and edit the conditions/contextual
# blocks to match how your callers actually supply request-time facts.
#
# See README.md (Glossary) and docs/configuration-reference.md for term
# definitions and a per-field reference.

`, modelPath)
}

func writeOpenFGA(b *strings.Builder) {
	b.WriteString(`openfga:
  # OpenFGA HTTP API URL. The bundled docker-compose.yaml publishes :8080.
  # Use http://host.docker.internal:8080 if you run fgaperf inside a container
  # that needs to reach the host's OpenFGA.
  api_url: http://localhost:8080

  # Name used when creating a fresh store for each run. The actual store ID
  # is recorded in .fgaperf-state.json; multiple runs can share a name and
  # ` + "`fgaperf cleanup -all-stores`" + ` deletes every store with this name.
  store_name: fgaperf

  # Pre-shared API token, if OpenFGA was started with
  # OPENFGA_AUTHN_METHOD=preshared. Leave unset for the docker-compose stack.
  # api_token: <secret>

  # Per-request HTTP timeout. Long checks against a slow datastore can exceed
  # the default; raise this before assuming the server is broken.
  timeout: 10s

`)
}

func writeTopLevel(b *strings.Builder, modelPath string) {
	fmt.Fprintf(b, `# Compiled model JSON. Export from a .fga file with `+"`fga model transform`."+`
# See README.md "Using Your Own Model" for the full procedure.
model_file: %s

# Where results-<stamp>.json, findings-<stamp>.md, and report-<stamp>.html are written.
output_dir: results

# Fixed seed makes tuple generation, probing, and request ordering
# reproducible across runs. Change the seed to confirm results are stable;
# repeat a value to reproduce a specific run.
random_seed: 42

# Uncomment to keep the store (and .fgaperf-state.json) after `+"`fgaperf all`"+`
# completes. Useful when you want to re-run `+"`probe`/`run`"+` against the same
# seeded data.
# keep_store: true

`, modelPath)
}

func writeSeed(b *strings.Builder, a *Analysis, instances map[string]int, fanout map[string]int, hasWildcards bool, contextualRels []string) {
	b.WriteString(`# ---------------------------------------------------------------------------
# seed: shape and size of the generated tuple graph
# ---------------------------------------------------------------------------
seed:
  # Number of tenant-like partitions. Tuples are biased to link within the
  # same cohort, so intersection and tuple-to-userset relations resolve to
  # "allowed" often enough to measure. Set roughly to the number of tenants
  # you want to simulate; 1 means a single global tenant.
  cohorts: 5

  # Default instance count for any type not listed in ` + "`instances`" + ` below.
  default_instances: 25

  # Per-type instance counts. Larger = more realistic graph, slower setup,
  # and bigger memory footprint while probing. User-like types (no relations
  # of their own) got bumped up here; container types stay at the default.
`)
	if len(instances) > 0 {
		b.WriteString("  instances:\n")
		for _, t := range sortedKeys(instances) {
			fmt.Fprintf(b, "    %s: %d\n", t, instances[t])
		}
	} else {
		b.WriteString("  # instances:\n  #   user: 250\n")
	}
	b.WriteString(`
  # Default fanout: tuples written per (object, relation, allowed user type).
  # Fanout 2 means each object gets 2 tuples per accepted user type. Higher
  # fanout = denser graph = more allowed checks and longer eval paths.
  default_fanout: 2

  # Per-relation fanout overrides, keyed "type#relation" or
  # "type#relation@usertype". The bare key applies to every accepted user
  # type; the @usertype suffix targets just that one (the bare key remains
  # the default for the others).
`)
	candidates := relationLevelFanoutCandidates(a, fanout, contextualRels)
	if len(fanout) > 0 || len(candidates) > 0 {
		b.WriteString("  fanout:\n")
		keys := sortedKeys(fanout)
		// Pad keys for readability across both the live entries and the
		// commented-out candidate list below.
		maxLen := 0
		for _, k := range keys {
			if len(k) > maxLen {
				maxLen = len(k)
			}
		}
		for _, k := range candidates {
			if len(k) > maxLen {
				maxLen = len(k)
			}
		}
		for _, k := range keys {
			fmt.Fprintf(b, "    %-*s %d\n", maxLen+1, k+":", fanout[k])
		}
		if len(candidates) > 0 {
			b.WriteString(`    # ---
    # Relation-level fanouts to consider. These relations accept direct
    # tuples but were left at default_fanout (2) — the operator usually
    # knows the right values (typical team size, container density, sharing
    # patterns) better than any heuristic. Uncomment and tune any that
    # should differ from the default so they don't fall through the cracks:
`)
			for _, k := range candidates {
				fmt.Fprintf(b, "    # %-*s 2\n", maxLen+1, k+":")
			}
		}
	} else {
		b.WriteString("  # fanout:\n  #   group#member: 8\n")
	}
	b.WriteString(`
  # Max tuples per Write API call. 100 is the OpenFGA server default cap;
  # raise only if your server is configured for larger batches.
  batch_size: 100

  # Concurrent Write workers during seeding. Raise to seed faster on a beefy
  # datastore; lower if you see write-side contention or 5xx errors during
  # the seed phase.
  writers: 8

`)
	if hasWildcards {
		b.WriteString(`  # Probability each object gets a wildcard (e.g. ` + "`user:*`" + `) tuple where the
  # model allows one. 0.5 means roughly half of objects are "public". Tune
  # this down if wildcard tuples are rare in your real data.
  wildcard_probability: 0.5

  # Per-relation overrides of the wildcard probability above.
  # wildcard_probabilities:
  #   document#viewer: 0.1   # only 10% of documents are public

`)
	} else {
		b.WriteString(`  # The model declares no wildcard user types; leave this at 0 (default).
  # wildcard_probability: 0

`)
	}
}

func writeContextual(b *strings.Builder, guesses []string) {
	b.WriteString(`# ---------------------------------------------------------------------------
# contextual: relations supplied per request instead of persisted as tuples
# ---------------------------------------------------------------------------
# These relations are NOT seeded. Probe records them on each corpus entry,
# and load sends them on each Check/BatchCheck request as ` + "`contextual_tuples`" + `.
# Use this for facts that only matter for one request (active session,
# current OAuth token, "in this device's geofence").
contextual:
`)
	if len(guesses) > 0 {
		b.WriteString("  # Guessed by name — review and remove anything that should be persisted.\n")
		b.WriteString("  relations:\n")
		for _, r := range guesses {
			fmt.Fprintf(b, "    - %s\n", r)
		}
		b.WriteString(`
  # Fraction of sampled checks that carry their contextual tuples.
  # 1.0 = every request has context (most realistic for first-party apps).
  # < 1.0 includes denied paths where the context was missing — useful for
  # measuring the cost of the missing-context fast-fail path.
  attach_probability: 1.0

`)
	} else {
		b.WriteString(`  # No relation names hinted at per-request facts. Uncomment and add any
  # relation whose tuples should ride on each Check rather than being
  # persisted during seeding.
  # relations:
  #   - document#active_context
  # attach_probability: 1.0

`)
	}
}

func writeConditions(b *strings.Builder, a *Analysis) {
	b.WriteString(`# ---------------------------------------------------------------------------
# conditions: how to generate CEL parameter values
# ---------------------------------------------------------------------------
# Default split: map/list parameters bound on the tuple, scalars supplied at
# check time. Override here when defaults don't match your conditions or you
# want to tune the allowed/denied mix CEL evaluation produces.
conditions:
`)
	names := make([]string, 0, len(a.Model.Conditions))
	for n := range a.Model.Conditions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := a.Model.Conditions[n]
		tupleSide, requestSide := splitConditionParams(c)
		fmt.Fprintf(b, "  %s:\n", n)
		fmt.Fprintf(b, "    # %s\n", strings.TrimSpace(c.Expression))
		if len(tupleSide) > 0 {
			b.WriteString("    # Which parameters are bound on the TUPLE (vs sent on the request).\n")
			b.WriteString("    # Maps and lists usually live on tuples; scalars usually live on the request.\n")
			fmt.Fprintf(b, "    tuple_params: [%s]\n", strings.Join(tupleSide, ", "))
		}
		if len(tupleSide)+len(requestSide) > 0 {
			b.WriteString("    params:\n")
		}
		for _, p := range tupleSide {
			pool := poolNameFor(n, p)
			fmt.Fprintf(b, "      %s:\n", p)
			fmt.Fprintf(b, "        pool: %s\n", pool)
			fmt.Fprintf(b, "        keys: 4               # entries per %s map/list\n", p)
		}
		for _, p := range requestSide {
			pool := poolNameFor(n, p)
			fmt.Fprintf(b, "      %s:\n", p)
			fmt.Fprintf(b, "        pool: %s            # scalar drawn from the named pool\n", pool)
		}
	}
	b.WriteString("\n")
}

func writePools(b *strings.Builder, a *Analysis) {
	b.WriteString(`# ---------------------------------------------------------------------------
# pools: named value pools used by condition parameters
# ---------------------------------------------------------------------------
pools:
`)
	type poolEntry struct{ name, prefix string }
	seen := map[string]bool{}
	var pools []poolEntry
	names := make([]string, 0, len(a.Model.Conditions))
	for n := range a.Model.Conditions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := a.Model.Conditions[n]
		params := make([]string, 0, len(c.Parameters))
		for p := range c.Parameters {
			params = append(params, p)
		}
		sort.Strings(params)
		for _, p := range params {
			pn := poolNameFor(n, p)
			if seen[pn] {
				continue
			}
			seen[pn] = true
			pools = append(pools, poolEntry{name: pn, prefix: p + "-"})
		}
	}
	for _, p := range pools {
		fmt.Fprintf(b, "  %s:\n", p.name)
		fmt.Fprintf(b, "    prefix: %q  # values generated as %s00, %s01, ...\n", p.prefix, p.prefix, p.prefix)
		fmt.Fprintf(b, "    count: 16\n")
		fmt.Fprintf(b, "    # Or supply explicit values:\n")
		fmt.Fprintf(b, "    # values: [a, b, c]\n")
	}
	b.WriteString("\n")
}

func writeNoConditionsPools(b *strings.Builder) {
	b.WriteString(`# The model declares no CEL conditions; no conditions/pools blocks needed.
# If you add a condition later, see examples/config.yaml for the layout.

`)
}

func writeProbe(b *strings.Builder, a *Analysis) {
	b.WriteString(`# ---------------------------------------------------------------------------
# probe: which checks to measure, and how the corpus is built
# ---------------------------------------------------------------------------
probe:
  # Relations to measure. Auto-populated with every relation in the model;
  # comment out the ones you don't care about, or add weights to skew the
  # load phase's traffic mix:
  #   - {relation: document#viewer, weight: 8}
  #   - {relation: document#editor, weight: 1}
  targets:
`)
	for _, tr := range a.AllRelations {
		fmt.Fprintf(b, "    - %s\n", tr.Key())
	}
	b.WriteString(`
  # Candidate samples per target before allowed/denied resampling. Raise to
  # smooth out variance in the per-target percentiles; lower for a faster probe.
  samples_per_target: 200

  # Probability a probe subject is drawn from the same cohort as the object.
  # Higher = more allowed paths resolve; lower = more cross-tenant denials.
  # 0.85 lands close to "real users mostly act within their own tenant".
  cohort_bias: 0.85

  # Desired allowed/denied mix in the final corpus. 0.5 = 50/50 split.
  # Use -1 to keep the natural mix observed during probing (recommended when
  # you want absolute throughput numbers; the resampling can over-represent
  # rare outcomes).
  allowed_ratio: 0.5

  # Concurrent probe workers. Higher = faster probe phase; lower = less
  # write/read contention on a constrained server.
  concurrency: 8

  # Attribute datastore queries per check per relation (needs metrics.prometheus_url).
  # Adds a "DS queries/check (probe)" column to the per-relation table — the sharpest
  # signal for an expensive rewrite. Best-effort; off by default.
  # attribute_ds_queries: true

`)
}

func writeLoad(b *strings.Builder) {
	b.WriteString(`# ---------------------------------------------------------------------------
# load: how the corpus is replayed
# ---------------------------------------------------------------------------
load:
  # check       = one (user, relation, object) per HTTP request.
  # batch-check = many tuples per request; trades per-request HTTP overhead
  #   for higher per-tuple throughput when your callers naturally batch.
  # list-objects / list-users = enumerate the allowed set instead of a yes/no.
  # endpoint also accepts a weighted blend so one run measures the contention
  # real services see when several endpoints share the server; the report then
  # splits percentiles per endpoint. For example:
  #   endpoint:
  #     check: 70
  #     list-objects: 20
  #     batch-check: 10
  endpoint: check

  # Items per batch-check request. Ignored unless batch-check is in the mix.
  batch_size: 20

  # Parallel workers issuing requests. Cap by how many concurrent Check
  # callers your real workload generates; raising this past saturation just
  # piles queueing into the same backend.
  concurrency: 16

  # Distinct non-negative load-generator ID for distributed runs. Set a unique
  # value per fgaperf process (or pass -client-id) so multiple clients replay
  # the same corpus with different request RNG streams, then combine the result
  # JSON files with ` + "`fgaperf merge`" + `.
  # client_id: 1

  # Fixed offered requests/sec. 0 = closed loop (each worker issues the next
  # request as soon as the previous returns; useful for finding saturation).
  # > 0 = pace the offered load at this rate (useful for measuring latency
  # at a realistic production load).
  rate: 0

  # Sweep instead of a single rate. The run steps through these offered
  # rates against the same store and corpus, then headlines the saturation
  # knee. Mutually exclusive with ` + "`rate`" + `.
  # sweep:
  #   rates: [200, 500, 1000, 2000, 4000]
  #   step_duration: 30s

  # Optional SLO target for response-latency p99. A sweep step counts as
  # "passing" only if response p99 is under this. Drop it to use the
  # default knee rule (achieved >= 98% of offered).
  # slo_p99: 25ms

  # Discarded leading slice — lets caches fill, connections warm up, etc.,
  # so the measured window reflects steady state. Keep at >= 5s for
  # meaningful Postgres-backed numbers.
  warmup: 10s

  # Measured window. Shorter runs are noisier; longer runs amortize tail
  # effects. 60s is a reasonable smoke; production-grade numbers usually
  # want 5m+.
  duration: 60s

  # For long single-rate soaks, emit cumulative interim results/findings at
  # this cadence while the final results/findings/report set still covers the
  # whole measured window. Also rotates sample_file into numbered chunks at the
  # same cadence.
  # Mutually exclusive with sweep. 0/off = only the final report.
  # report_interval: 5m

  # MINIMIZE_LATENCY = cached reads when possible (typical production
  # default; matches what fast callers see).
  # HIGHER_CONSISTENCY = skip caches every time (matches "must be fresh"
  # callers like UI permission decisions immediately after a write).
  consistency: MINIMIZE_LATENCY

  # Compare each load-time response against the probe-time ground truth.
  # Mismatches almost always point at cache staleness or consistency mode;
  # disable only if you intentionally seed/run with inconsistent settings.
  verify_results: true

  # Background tuple writes per second during the measured phase, so checks
  # run against a churning store instead of the read-only best case. The
  # churn writes only touch fresh churn-only instances, so verify_results
  # still works. 0 = no churn.
  # write_rate: 50

  # Dump one JSON line per measured sample (target, latency, response latency,
  # outcome class, timestamp) for your own analysis. A .gz suffix gzips the
  # stream. A sweep's steps all append to the same file; with report_interval,
  # each interval writes a numbered sample chunk. Off when unset.
  # sample_file: samples.jsonl.gz

`)
}

func writeMetrics(b *strings.Builder) {
	b.WriteString(`# ---------------------------------------------------------------------------
# metrics: optional server-side view
# ---------------------------------------------------------------------------
metrics:
  # OpenFGA Prometheus endpoint. The bundled compose stack publishes :2112.
  # When set, the findings doc gains a server-side view (request duration,
  # datastore queries per check, dispatches, cache hits) diffed over the
  # measured phase only. On shared OpenFGA deployments, these counters may
  # include unrelated traffic unless labels let you isolate this run.
  # Unreachable URL = section skipped.
  prometheus_url: http://localhost:2112
`)
}

// --- heuristics ---------------------------------------------------------

// pickInstances assigns instance counts per type. Types with no relations of
// their own are "user-like" — they appear as Check subjects rather than as
// objects with their own grants. Real systems have many more users than
// containers, so bump them up. Container types stay near the default so the
// emitted YAML reads as "the user types got tuned, everything else is the
// default".
func pickInstances(a *Analysis) map[string]int {
	out := map[string]int{}
	for _, t := range a.Types {
		td := a.TypeDefs[t]
		if td == nil {
			continue
		}
		if len(td.Relations) == 0 {
			out[t] = 250
		} else {
			out[t] = 50
		}
	}
	return out
}

// pickFanout suggests per-relation fanout overrides. Two patterns get
// special treatment:
//
//  1. Userset acceptors (e.g. group#member). When a relation accepts a
//     userset as a user type, the only way for membership to resolve is
//     through that linked relation. Treating fanout 2 there as the default
//     leaves group#member with two users per group — too few for a
//     production-shaped graph. Bump those user-type fanouts to 8.
//
//  2. Self-referential relations (e.g. folder#parent pointing to folder).
//     The generator only links higher-indexed instances to lower ones to
//     avoid cycles; fanout > 1 there just compresses the resulting DAG
//     without adding interesting paths. Pin to 1.
func pickFanout(a *Analysis) map[string]int {
	out := map[string]int{}
	for _, tr := range a.AllRelations {
		refs := a.DirectRefs[tr.Type][tr.Relation]
		if len(refs) == 0 {
			continue
		}
		key := tr.Key()
		selfReferential := false
		for _, r := range refs {
			if r.Type == tr.Type && r.Relation == "" && r.Wildcard == nil {
				selfReferential = true
			}
		}
		if selfReferential && len(refs) == 1 {
			out[key] = 1
			continue
		}
		// Per-user-type bumps for userset acceptors.
		for _, r := range refs {
			if r.Relation == "" {
				continue
			}
			usertype := r.Type + "#" + r.Relation
			out[key+"@"+usertype] = 8
		}
	}
	return out
}

// relationLevelFanoutCandidates lists assignable relations whose bare
// "type#relation" fanout key isn't already set by the auto-picked overrides,
// excluding contextual relations (which aren't seeded). These are the
// "broad strokes" knobs the operator should consciously decide on instead
// of accepting default_fanout. Producing this list as a commented block
// inside the fanout map is what keeps them from falling through the cracks
// on the auto-generated path.
func relationLevelFanoutCandidates(a *Analysis, fanout map[string]int, contextualRels []string) []string {
	skip := map[string]bool{}
	for _, r := range contextualRels {
		skip[r] = true
	}
	for k := range fanout {
		if !strings.Contains(k, "@") {
			skip[k] = true
		}
	}
	var out []string
	for _, tr := range a.AllRelations {
		key := tr.Key()
		if skip[key] {
			continue
		}
		// Only relations that actually accept direct tuples are tunable via
		// fanout; computed-only relations are skipped.
		assignable := false
		for _, ref := range a.DirectRefs[tr.Type][tr.Relation] {
			if ref.Wildcard == nil {
				assignable = true
				break
			}
		}
		if assignable {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func analysisHasWildcards(a *Analysis) bool {
	for _, rels := range a.DirectRefs {
		for _, refs := range rels {
			for _, r := range refs {
				if r.Wildcard != nil {
					return true
				}
			}
		}
	}
	return false
}

func guessContextualRelations(a *Analysis) []string {
	var out []string
	for _, tr := range a.AllRelations {
		if contextualNameRE.MatchString(tr.Relation) && hasPlainDirectRef(a.DirectRefs[tr.Type][tr.Relation]) {
			out = append(out, tr.Key())
		}
	}
	return out
}

// splitConditionParams mirrors Analysis.TupleContextParams' default policy
// without depending on a Config: map/list parameters ride on the tuple,
// scalars ride on the request.
func splitConditionParams(c Condition) (tupleSide, requestSide []string) {
	names := make([]string, 0, len(c.Parameters))
	for p := range c.Parameters {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		switch c.Parameters[p].TypeName {
		case "TYPE_NAME_MAP", "TYPE_NAME_LIST":
			tupleSide = append(tupleSide, p)
		default:
			requestSide = append(requestSide, p)
		}
	}
	return
}

func poolNameFor(condition, param string) string {
	return condition + "_" + param
}
