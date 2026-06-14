package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

type commandDoc struct {
	Summary string
	Details string
	Flags   []string
	Example string
	Gotcha  string
}

var commandDocs = map[string]commandDoc{
	"inspect": {
		Summary: "print the model analysis; no server needed",
		Details: "Loads the configured compiled model JSON, validates model-named config keys, and prints every relation with tags for assignable, CEL-reachable, and contextual paths.",
		Flags:   []string{"config", "json"},
		Example: "./fgaperf inspect -config examples/config.yaml\n./fgaperf inspect -config examples/config.yaml -json",
	},
	"plan": {
		Summary: "preview generated graph size and load budget; no server needed",
		Details: "Prints the resolved config, per-type instance counts, estimated tuple counts, probe budget, load duration, and warnings for likely-empty probe targets.",
		Flags:   []string{"config"},
		Example: "./fgaperf plan -config examples/config.yaml",
	},
	"validate": {
		Summary: "validate config and print the redacted resolved config; no server needed",
		Details: "Runs strict YAML/config validation plus model-name validation, then prints the post-defaults config that results will embed.",
		Flags:   []string{"config"},
		Example: "./fgaperf validate -config examples/config.yaml",
	},
	"doctor": {
		Summary: "run pre-flight checks against OpenFGA and optional metrics",
		Details: "Checks model parsing, config/model compatibility, HTTP reachability, temporary store create/delete, model write, and metrics families when metrics.prometheus_url is set.",
		Flags:   []string{"config"},
		Example: "./fgaperf doctor -config examples/config.yaml",
		Gotcha:  "With the bundled stack stopped, run `docker compose up -d` and then `docker compose ps`.",
	},
	"setup": {
		Summary: "create a store, write the model, and seed tuples",
		Details: "Generates the deterministic tuple graph, creates a fresh store, writes the model, seeds tuples, and writes the state file used by probe/run.",
		Flags:   []string{"config", "resume"},
		Example: "./fgaperf setup -config examples/config.yaml\n./fgaperf setup -config examples/config.yaml -resume",
		Gotcha:  "Writes `.fgaperf-state.json`; re-running without -resume creates a fresh store.",
	},
	"probe": {
		Summary: "build corpus.json from probe-time ground truth",
		Details: "Requires setup to have run. Samples configured targets, checks each candidate once with HIGHER_CONSISTENCY, then writes the replay corpus. With corpus_source: replay it instead reads a real check log (replay.file), still learning each distinct entry's ground truth, and weights the load mix by the log's per-target frequencies.",
		Flags:   []string{"config"},
		Example: "./fgaperf probe -config examples/config.yaml",
		Gotcha:  "Reads model + state, writes `corpus.json`; rerun after changing seed/probe config.",
	},
	"run": {
		Summary: "replay corpus under load and write results",
		Details: "Requires setup and probe. Replays corpus.json using the configured endpoint, concurrency, rate/sweep, warmup, duration, consistency, and repeat count.",
		Flags:   []string{"config", "duration", "warmup", "rate", "concurrency", "client-id", "repeat", "endpoint", "consistency", "transport", "output-dir"},
		Example: "./fgaperf run -config examples/config.yaml -duration 30s\n./fgaperf run -config examples/config.yaml -rate 1000 -duration 1m\n./fgaperf run -config examples/config.yaml -repeat 5 -output-dir results/a\n./fgaperf run -config examples/config.yaml -client-id 2 -output-dir results/client-2",
		Gotcha:  "`load.rate` and `load.sweep.rates` are mutually exclusive.",
	},
	"all": {
		Summary: "setup, probe, run, and cleanup in one command",
		Details: "The usual one-shot workflow. Creates a fresh store, builds a corpus, runs load, writes results, and deletes the store unless -keep or keep_store is set.",
		Flags:   []string{"config", "keep", "duration", "warmup", "rate", "concurrency", "client-id", "repeat", "endpoint", "consistency", "transport", "output-dir"},
		Example: "./fgaperf all -config examples/config.yaml -warmup 2s -duration 8s\n./fgaperf all -config examples/config.yaml -repeat 5",
		Gotcha:  "Use -keep when you want to rerun probe/run against the same seeded store.",
	},
	"cleanup": {
		Summary: "delete the recorded store, or all stores with the configured name",
		Details: "Deletes the store recorded in the state file. With -all-stores, lists stores and deletes every store whose name matches openfga.store_name.",
		Flags:   []string{"config", "all-stores"},
		Example: "./fgaperf cleanup -config examples/config.yaml\n./fgaperf cleanup -config examples/config.yaml -all-stores",
		Gotcha:  "`-all-stores` deletes by name, not by ID; use it when the state file is gone.",
	},
	"compare": {
		Summary: "render two results JSON files side by side, or gate against a baseline",
		Details: "Writes a Markdown comparison with overall/per-relation deltas, server-side deltas, config differences, and comparability caveats. For repeated runs, pass two file sets separated by `:` to report mean +/- stdev and label each delta significant or within noise. With -against-baseline, compares one results JSON to a compact saved baseline and exits non-zero when any configured regression threshold is exceeded (the CI regression gate; pass -exit-on-regression=false for an advisory, non-blocking comparison).",
		Flags:   []string{"config", "output-dir", "against-baseline", "max-regression", "exit-on-regression"},
		Example: "./fgaperf compare -config examples/config.yaml results/results-A.json results/results-B.json\n./fgaperf compare results/a/*.json : results/b/*.json\n./fgaperf compare -against-baseline results/baseline.json -max-regression p99=10%,throughput=-5% results/results-new.json",
	},
	"merge": {
		Summary: "combine digest-enabled results from multiple load generators",
		Details: "Reads two or more single-rate results JSON files produced against the same store and corpus, merges their latency digests, sums concurrency/offered/achieved throughput, and writes one combined results JSON plus findings Markdown. Use distinct load.client_id values (or -client-id) for each generator so their request RNG streams differ.",
		Flags:   []string{"config", "output-dir"},
		Example: "./fgaperf merge -output-dir results/merged results/client-*/results-*.json",
		Gotcha:  "Merge currently supports single-rate reports; sweep reports should be compared separately.",
	},
	"baseline": {
		Summary: "save a compact regression baseline from a results JSON",
		Details: "Reads a fgaperf results JSON and writes a compact baseline JSON containing the run shape, config fingerprint, random seed, throughput, key latency percentiles, per-target p99 inputs, and server-side datastore cost when present.",
		Flags:   []string{"config", "output-dir"},
		Example: "./fgaperf baseline save results/results-20260613-203758.json\n./fgaperf baseline save -output-dir results/baselines results/results-20260613-203758.json",
	},
	"gen-config": {
		Summary: "emit an annotated starter config from a compiled model",
		Details: "Reads a compiled model JSON directly and writes a commented config to stdout or -o.",
		Flags:   []string{"model", "o", "force"},
		Example: "./fgaperf gen-config -model model.json > config.yaml\n./fgaperf gen-config -model model.json -o config.yaml -force",
		Gotcha:  "Use `fga model transform --file model.fga > model.json` when starting from DSL.",
	},
}

func printRootHelp(w io.Writer) {
	fmt.Fprintln(w, "fgaperf: model-driven performance testing for OpenFGA")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  fgaperf <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	names := make([]string, 0, len(commandDocs))
	for name := range commandDocs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-10s %s\n", name, commandDocs[name].Summary)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Run `fgaperf <command> -h` for command-specific flags and examples.")
}

func printCommandHelp(w io.Writer, cmd string, fs *flag.FlagSet) {
	doc, ok := commandDocs[cmd]
	if !ok {
		fmt.Fprintf(w, "usage: fgaperf %s [flags]\n", cmd)
		fs.PrintDefaults()
		return
	}
	fmt.Fprintf(w, "fgaperf %s: %s\n\n", cmd, doc.Summary)
	if doc.Details != "" {
		fmt.Fprintln(w, wrapHelp(doc.Details))
		fmt.Fprintln(w, "")
	}
	fmt.Fprintln(w, "Usage:")
	if cmd == "compare" {
		fmt.Fprintln(w, "  fgaperf compare [flags] <results-a.json> <results-b.json>")
		fmt.Fprintln(w, "  fgaperf compare -against-baseline <baseline.json> [flags] <results.json>")
	} else if cmd == "baseline" {
		fmt.Fprintln(w, "  fgaperf baseline [flags] save [save-flags] <results.json>")
	} else if cmd == "merge" {
		fmt.Fprintln(w, "  fgaperf merge [flags] <results-a.json> <results-b.json> [...]")
	} else {
		fmt.Fprintf(w, "  fgaperf %s [flags]\n", cmd)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	printSelectedFlags(w, fs, doc.Flags)
	if doc.Example != "" {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Examples:")
		for _, line := range strings.Split(doc.Example, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if doc.Gotcha != "" {
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "Gotcha: %s\n", doc.Gotcha)
	}
}

func wrapHelp(s string) string {
	return s
}

func printSelectedFlags(w io.Writer, fs *flag.FlagSet, names []string) {
	if len(names) == 0 {
		fs.SetOutput(w)
		fs.PrintDefaults()
		return
	}
	for _, name := range names {
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		def := ""
		if f.DefValue != "" && f.DefValue != "0" && f.DefValue != "false" {
			def = fmt.Sprintf(" (default %q)", f.DefValue)
		}
		fmt.Fprintf(w, "  -%s%s\n    \t%s\n", f.Name, def, f.Usage)
	}
}
