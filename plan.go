package main

// plan.go implements `fgaperf plan`: a server-free preview of the tuple graph a
// config would seed. It lets users sanity-check graph size before committing to
// a long seed, and iterate on fanout/instances/cohorts without a round trip to
// OpenFGA.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

func validateOnly(a *Analysis, cfg *Config) {
	validateReport(a, cfg, os.Stdout)
}

func validateReport(a *Analysis, cfg *Config, w io.Writer) {
	fmt.Fprintf(w, "config valid for %s (no server contacted)\n\n", cfg.ModelFile)
	writeResolvedConfig(w, cfg)
	for _, warning := range planWarnings(a, cfg) {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
}

// planReport writes the plan to w (split out so it is testable).
func planReport(a *Analysis, cfg *Config, w io.Writer) {
	fmt.Fprintf(w, "plan for %s (no server contacted)\n\n", cfg.ModelFile)
	writeResolvedConfig(w, cfg)

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
	fmt.Fprintf(w, "final corpus will contain up to %d entries before allowed/denied resampling; scarce outcomes may be duplicated or kept at their natural mix.\n",
		probeBudget)
	loadDur := cfg.Load.Warmup + cfg.Load.Duration
	if len(cfg.Load.Sweep.Rates) > 0 {
		loadDur = cfg.Load.Warmup + timeMul(cfg.Load.Sweep.StepDuration, len(cfg.Load.Sweep.Rates))
	}
	fmt.Fprintf(w, "load phase time budget: %s (warmup plus measured windows; excludes setup/probe/server time).\n", loadDur)

	warnings := planWarnings(a, cfg)
	if len(warnings) > 0 {
		fmt.Fprintln(w, "\nwarnings:")
		for _, warning := range warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}

	fmt.Fprintln(w, "\nNote: tuple counts are an upper bound — actual seeding de-duplicates,")
	fmt.Fprintln(w, "skips empty cohorts, and limits self-referential relations to lower-index")
	fmt.Fprintln(w, "subjects, so the real total is usually lower. Run `fgaperf setup` to seed.")
}

func writeResolvedConfig(w io.Writer, cfg *Config) {
	fmt.Fprintln(w, "resolved config (credentials redacted):")
	data, err := yaml.Marshal(cfg.Resolved())
	if err != nil {
		fmt.Fprintf(w, "  <unavailable: %v>\n\n", err)
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
	fmt.Fprintln(w, "")
}

func planWarnings(a *Analysis, cfg *Config) []string {
	targets := cfg.Probe.Targets
	if len(targets) == 0 {
		for _, tr := range a.AllRelations {
			targets = append(targets, TargetSpec{Relation: tr.Key(), Weight: 1})
		}
	}
	var warnings []string
	if len(a.SubjectTypes) == 0 && len(cfg.Probe.SubjectTypes) == 0 {
		warnings = append(warnings, "no terminal subject types were inferred; set probe.subject_types explicitly")
	}
	for _, t := range targets {
		typ, rel, ok := strings.Cut(t.Relation, "#")
		if !ok {
			continue
		}
		if !relationHasAssignablePath(a, typ, rel, map[string]bool{}) {
			warnings = append(warnings, fmt.Sprintf("probe target %s has no reachable direct tuple path; probe is likely to produce only denied or empty entries", t.Relation))
		}
	}
	return warnings
}

func relationHasAssignablePath(a *Analysis, typ, rel string, seen map[string]bool) bool {
	key := typ + "#" + rel
	if seen[key] {
		return false
	}
	seen[key] = true
	if len(a.DirectRefs[typ][rel]) > 0 {
		return true
	}
	td := a.TypeDefs[typ]
	if td == nil {
		return false
	}
	us, ok := td.Relations[rel]
	if !ok {
		return false
	}
	return usersetHasAssignablePath(a, typ, &us, seen)
}

func usersetHasAssignablePath(a *Analysis, typ string, us *Userset, seen map[string]bool) bool {
	switch {
	case us.This != nil:
		return false
	case us.ComputedUserset != nil:
		return relationHasAssignablePath(a, typ, us.ComputedUserset.Relation, seen)
	case us.TupleToUserset != nil:
		if relationHasAssignablePath(a, typ, us.TupleToUserset.Tupleset.Relation, seen) {
			return true
		}
		for _, ref := range a.DirectRefs[typ][us.TupleToUserset.Tupleset.Relation] {
			if relationHasAssignablePath(a, ref.Type, us.TupleToUserset.ComputedUserset.Relation, seen) {
				return true
			}
		}
	case us.Union != nil:
		for i := range us.Union.Child {
			if usersetHasAssignablePath(a, typ, &us.Union.Child[i], seen) {
				return true
			}
		}
	case us.Intersection != nil:
		for i := range us.Intersection.Child {
			if usersetHasAssignablePath(a, typ, &us.Intersection.Child[i], seen) {
				return true
			}
		}
	case us.Difference != nil:
		return usersetHasAssignablePath(a, typ, &us.Difference.Base, seen) ||
			usersetHasAssignablePath(a, typ, &us.Difference.Subtract, seen)
	}
	return false
}

func timeMul(d time.Duration, n int) time.Duration {
	return d * time.Duration(n)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
