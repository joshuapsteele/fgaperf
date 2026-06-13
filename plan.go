package main

// plan.go implements `fgaperf plan`: a server-free preview of the tuple graph a
// config would seed. It lets users sanity-check graph size before committing to
// a long seed, and iterate on fanout/instances/cohorts without a round trip to
// OpenFGA.

import (
	"fmt"
	"io"
	"os"
)

// planInstanceCounts returns the per-type instance count the seeder would
// create, applying the per-type override or the default.
func planInstanceCounts(a *Analysis, cfg *Config) map[string]int {
	out := map[string]int{}
	for _, t := range a.Types {
		n := cfg.Seed.DefaultCount
		if v, ok := cfg.Seed.Instances[t]; ok {
			n = v
		}
		out[t] = n
	}
	return out
}

// planTupleCounts estimates per-relation seeded tuple counts from config alone,
// mirroring GenerateTuples' fanout logic without generating anything. It is an
// upper bound: de-duplication, empty-cohort breaks, and the lower-index limit
// on self-referential relations can only reduce the real count. Contextual
// relations are excluded (their tuples ride on requests, not the store).
func planTupleCounts(a *Analysis, cfg *Config) map[string]int {
	contextual := contextualSet(cfg)
	inst := planInstanceCounts(a, cfg)
	out := map[string]int{}
	for typ, rels := range a.DirectRefs {
		for relName, refs := range rels {
			key := typ + "#" + relName
			if contextual[key] {
				continue
			}
			objs := inst[typ]
			relFanout := cfg.Seed.DefaultFanout
			if v, ok := cfg.Seed.Fanout[key]; ok {
				relFanout = v
			}
			wp := cfg.Seed.WildcardProb
			if v, ok := cfg.Seed.WildcardProbs[key]; ok {
				wp = v
			}
			total := 0
			for _, ref := range refs {
				if ref.Wildcard != nil {
					total += int(float64(objs) * wp) // ~one wildcard tuple per object, with probability wp
					continue
				}
				fanout := relFanout
				suffix := ref.Type
				if ref.Relation != "" {
					suffix += "#" + ref.Relation
				}
				if v, ok := cfg.Seed.Fanout[key+"@"+suffix]; ok {
					fanout = v
				}
				total += objs * fanout
			}
			out[key] = total
		}
	}
	return out
}

func plan(a *Analysis, cfg *Config) {
	planReport(a, cfg, os.Stdout)
}

// planReport writes the plan to w (split out so it is testable).
func planReport(a *Analysis, cfg *Config, w io.Writer) {
	fmt.Fprintf(w, "plan for %s (no server contacted)\n\n", cfg.ModelFile)

	inst := planInstanceCounts(a, cfg)
	fmt.Fprintf(w, "instances per type (%d cohorts):\n", cfg.Seed.Cohorts)
	totalInst := 0
	for _, t := range sortedKeys(inst) {
		fmt.Fprintf(w, "  %-24s %8d\n", t, inst[t])
		totalInst += inst[t]
	}
	fmt.Fprintf(w, "  %-24s %8d\n", "TOTAL", totalInst)

	tuples := planTupleCounts(a, cfg)
	fmt.Fprintln(w, "\nestimated seeded tuples per relation (upper bound):")
	totalTuples := 0
	for _, k := range sortedKeys(tuples) {
		if tuples[k] == 0 {
			continue
		}
		fmt.Fprintf(w, "  %-24s %8d\n", k, tuples[k])
		totalTuples += tuples[k]
	}
	fmt.Fprintf(w, "  %-24s %8d\n", "TOTAL", totalTuples)

	probeBudget := len(cfg.Probe.Targets)
	if probeBudget == 0 {
		probeBudget = len(a.AllRelations)
	}
	probeBudget *= cfg.Probe.Samples
	fmt.Fprintf(w, "\nprobe will execute up to %d candidate checks (%d targets x %d samples_per_target).\n",
		probeBudget, probeBudget/maxInt(cfg.Probe.Samples, 1), cfg.Probe.Samples)

	fmt.Fprintln(w, "\nNote: tuple counts are an upper bound — actual seeding de-duplicates,")
	fmt.Fprintln(w, "skips empty cohorts, and limits self-referential relations to lower-index")
	fmt.Fprintln(w, "subjects, so the real total is usually lower. Run `fgaperf setup` to seed.")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
