package main

// fgaperf: model-driven performance testing for OpenFGA.
//
//	fgaperf setup -config config.yaml   create store, write model, seed tuples
//	fgaperf probe -config config.yaml   build the check corpus
//	fgaperf run   -config config.yaml   run the load test, write report
//	fgaperf all   -config config.yaml   all of the above, then delete the store
//	fgaperf inspect -config config.yaml print the model analysis and exit
//	fgaperf cleanup -config config.yaml delete the store recorded in the state file
//	fgaperf compare a.json b.json       render two results files side by side
//	fgaperf gen-config -model model.json  emit a starter config.yaml on stdout

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

type State struct {
	StoreID      string    `json:"store_id"`
	ModelID      string    `json:"model_id"`
	TupleCount   int       `json:"tuple_count"`
	SeedDuration string    `json:"seed_duration"`
	SeededAt     time.Time `json:"seeded_at"`
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: fgaperf <setup|probe|run|all|inspect|cleanup|compare|gen-config> [-config config.yaml]")
	}
	cmd := os.Args[1]
	// gen-config is the one command that takes neither -config nor a loaded
	// model from the configured path; it operates directly on a model file
	// and writes annotated YAML to stdout (or -o path). Handled inline so the
	// shared flag block below doesn't need to learn its quirks.
	if cmd == "gen-config" {
		runGenConfig(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file (optional; defaults apply)")
	keep := fs.Bool("keep", false, "all: keep the store and state file instead of deleting them")
	allStores := fs.Bool("all-stores", false, "cleanup: delete every store whose name matches openfga.store_name")
	// Common load knobs as flags so a quick run doesn't need a config edit (or
	// the sed dance the README used to recommend). Only flags actually passed
	// override the config; defaults here are inert sentinels.
	durFlag := fs.Duration("duration", 0, "override load.duration")
	warmupFlag := fs.Duration("warmup", 0, "override load.warmup")
	rateFlag := fs.Int("rate", 0, "override load.rate (req/s; 0 = closed loop)")
	concFlag := fs.Int("concurrency", 0, "override load.concurrency")
	endpointFlag := fs.String("endpoint", "", "override load.endpoint (check|batch-check)")
	consistencyFlag := fs.String("consistency", "", "override load.consistency (MINIMIZE_LATENCY|HIGHER_CONSISTENCY)")
	outDirFlag := fs.String("output-dir", "", "override output_dir")
	fs.Parse(os.Args[2:])

	cfgFile := *cfgPath
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "config %s not found, using built-in defaults\n", cfgFile)
		cfgFile = ""
	}
	cfg, err := LoadConfigFile(cfgFile)
	check(err)

	var ov Overrides
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "duration":
			ov.Duration = durFlag
		case "warmup":
			ov.Warmup = warmupFlag
		case "rate":
			ov.Rate = rateFlag
		case "concurrency":
			ov.Concurrency = concFlag
		case "endpoint":
			ov.Endpoint = endpointFlag
		case "consistency":
			ov.Consistency = consistencyFlag
		case "output-dir":
			ov.OutputDir = outDirFlag
		}
	})
	if err := cfg.applyOverrides(ov); err != nil {
		fail("invalid flag override: %v", err)
	}

	// run and cleanup operate on an existing store and never read the model.
	var analysis *Analysis
	switch cmd {
	case "inspect", "setup", "probe", "all":
		analysis, err = LoadModel(cfg.ModelFile)
		if err != nil {
			fail("%v\nfgaperf needs a compiled OpenFGA authorization model; set model_file in the config (see examples/)", err)
		}
		if err := cfg.validateAgainstModel(analysis); err != nil {
			fail("invalid config: %v", err)
		}
	}

	client := NewFGAClient(cfg.OpenFGA, cfg.Load.Concurrency)

	switch cmd {
	case "inspect":
		inspect(analysis, cfg)
	case "setup":
		_, err := setup(client, analysis, cfg)
		check(err)
	case "probe":
		st := loadState(cfg)
		check(probe(client, analysis, cfg, st))
	case "run":
		st := loadState(cfg)
		check(run(client, cfg, st))
	case "all":
		check(runAll(client, analysis, cfg, *keep || cfg.KeepStore))
	case "cleanup":
		check(cleanup(client, cfg, *allStores))
	case "compare":
		args := fs.Args()
		if len(args) != 2 {
			fail("usage: fgaperf compare <results-a.json> <results-b.json>")
		}
		check(compare(args[0], args[1], cfg.OutputDir))
	default:
		fail("unknown command %q", cmd)
	}
}

// runAll executes setup, probe, and run, then deletes the store it created so
// repeated runs against a deployed OpenFGA leave nothing behind. The store is
// also deleted when a phase fails or the process is interrupted.
func runAll(client *FGAClient, a *Analysis, cfg *Config, keep bool) error {
	st, err := setup(client, a, cfg)
	if err != nil {
		return err
	}
	if !keep {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigs
			fmt.Fprintln(os.Stderr, "\ninterrupted, deleting store...")
			deleteStore(client, cfg, st)
			os.Exit(1)
		}()
		defer func() {
			signal.Stop(sigs)
			deleteStore(client, cfg, st)
		}()
	}
	if err := probe(client, a, cfg, st); err != nil {
		return err
	}
	return run(client, cfg, st)
}

// deleteStore removes the store and the state file pointing at it. Failure is
// reported but not fatal: the test results are already on disk.
func deleteStore(client *FGAClient, cfg *Config, st *State) {
	if err := client.DeleteStore(st.StoreID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: deleting store %s failed: %v\nrun `fgaperf cleanup` to retry\n", st.StoreID, err)
		return
	}
	os.Remove(cfg.StateFile)
	fmt.Printf("store deleted: %s\n", st.StoreID)
}

func cleanup(client *FGAClient, cfg *Config, allStores bool) error {
	if !allStores {
		data, err := os.ReadFile(cfg.StateFile)
		if err != nil {
			return fmt.Errorf("no state file %s (nothing to clean up?); use -all-stores to delete by store name: %w", cfg.StateFile, err)
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
		deleteStore(client, cfg, &st)
		return nil
	}
	stores, err := client.ListStores()
	if err != nil {
		return fmt.Errorf("listing stores: %w", err)
	}
	n := 0
	for _, s := range stores {
		if s.Name != cfg.OpenFGA.StoreName {
			continue
		}
		if err := client.DeleteStore(s.ID); err != nil {
			return fmt.Errorf("deleting store %s: %w", s.ID, err)
		}
		fmt.Printf("store deleted: %s (%s)\n", s.ID, s.Name)
		n++
	}
	if n == 0 {
		fmt.Printf("no stores named %q found\n", cfg.OpenFGA.StoreName)
	} else {
		os.Remove(cfg.StateFile)
	}
	return nil
}

func inspect(a *Analysis, cfg *Config) {
	fmt.Printf("model: %d types, %d relations, %d conditions\n",
		len(a.Types), len(a.AllRelations), len(a.Model.Conditions))
	fmt.Printf("inferred subject types: %v\n", a.SubjectTypes)
	fmt.Println("  (subject types are the leaf user types Check questions resolve down to —")
	fmt.Println("   probe samples its `user` from these unless you set probe.subject_types)")
	contextual := contextualSet(cfg)
	fmt.Println("")
	fmt.Println("relations:")
	fmt.Println("  tag legend:")
	fmt.Println("    —             this relation has no CEL condition on any path")
	fmt.Println("    CEL           at least one resolution path can evaluate a CEL condition")
	fmt.Println("    [assignable]  accepts direct tuples (you can Write `<obj>#<rel>@<user>`)")
	fmt.Println("    [contextual]  fgaperf will send this relation's tuples on each Check")
	fmt.Println("                  request (configured via contextual.relations) instead of")
	fmt.Println("                  persisting them during setup")
	fmt.Println("")
	for _, tr := range a.AllRelations {
		tag := "—"
		if a.Conditioned[tr.Key()] {
			tag = "CEL"
		}
		direct := ""
		if len(a.DirectRefs[tr.Type][tr.Relation]) > 0 {
			direct = " [assignable]"
		}
		if contextual[tr.Key()] {
			direct += " [contextual]"
		}
		fmt.Printf("  %-4s %s%s\n", tag, tr.Key(), direct)
	}
	if len(a.Model.Conditions) > 0 {
		fmt.Println("")
		fmt.Println("conditions:")
		fmt.Println("  tuple-side params live on the stored tuple (e.g. \"granted_scopes\");")
		fmt.Println("  request-side params are sent by Check at evaluation time (e.g. \"required_scope\").")
		fmt.Println("  Override the split with the `conditions` block in your config.")
		fmt.Println("")
		for name, c := range a.Model.Conditions {
			ts, rs := a.TupleContextParams(name, cfg)
			fmt.Printf("  %s: %q\n", name, c.Expression)
			fmt.Printf("    tuple-side params:   %v\n", ts)
			fmt.Printf("    request-side params: %v\n", rs)
		}
	}
}

func setup(client *FGAClient, a *Analysis, cfg *Config) (*State, error) {
	storeID, err := client.CreateStore(cfg.OpenFGA.StoreName)
	if err != nil {
		return nil, fmt.Errorf("creating store: %w", err)
	}
	fmt.Printf("store created: %s\n", storeID)
	modelID, err := client.WriteModel(storeID, a.RawModel)
	if err != nil {
		return nil, fmt.Errorf("writing model: %w", err)
	}
	fmt.Printf("model written: %s\n", modelID)

	world := NewWorld(a, cfg)
	tuples := world.GenerateTuples()
	fmt.Printf("generated %d tuples across %d cohorts\n", len(tuples), cfg.Seed.Cohorts)
	dur, err := SeedStore(client, storeID, modelID, tuples, cfg)
	if err != nil {
		return nil, fmt.Errorf("seeding: %w", err)
	}
	fmt.Printf("seeded in %s (%.0f tuples/sec)\n", dur.Round(time.Millisecond), float64(len(tuples))/dur.Seconds())

	st := &State{StoreID: storeID, ModelID: modelID, TupleCount: len(tuples), SeedDuration: dur.String(), SeededAt: time.Now().UTC()}
	data, _ := json.MarshalIndent(st, "", " ")
	if err := os.WriteFile(cfg.StateFile, data, 0o644); err != nil {
		return nil, err
	}
	return st, nil
}

func probe(client *FGAClient, a *Analysis, cfg *Config, st *State) error {
	world := NewWorld(a, cfg) // deterministic: same seed regenerates the same instance space
	corpus, err := BuildCorpus(client, a, world, cfg, st.StoreID, st.ModelID)
	if err != nil {
		return err
	}
	var allowed int
	condCount := 0
	contextualCount := 0
	for _, e := range corpus.Entries {
		if e.Expected {
			allowed++
		}
		if e.Conditioned {
			condCount++
		}
		if e.Contextual {
			contextualCount++
		}
	}
	fmt.Printf("corpus: %d entries (%d distinct checks; %d allowed / %d denied; %d on CEL-conditioned paths; %d with contextual tuples)\n",
		len(corpus.Entries), corpus.Distinct(), allowed, len(corpus.Entries)-allowed, condCount, contextualCount)
	for _, target := range sortedKeys(corpus.Stats) {
		st := corpus.Stats[target]
		if st.Distinct*2 < st.Total { // more than 2x average duplication is worth a look
			fmt.Fprintf(os.Stderr, "probe: target %s corpus is %d entries but only %d distinct checks (%.1fx duplication); cache hit rates under load will be inflated\n",
				target, st.Total, st.Distinct, float64(st.Total)/float64(st.Distinct))
		}
	}
	return corpus.Save(cfg.CorpusFile)
}

func run(client *FGAClient, cfg *Config, st *State) error {
	corpus, err := LoadCorpus(cfg.CorpusFile)
	if err != nil {
		return fmt.Errorf("loading corpus (run probe first?): %w", err)
	}
	fmt.Printf("load: endpoint=%s concurrency=%d rate=%v warmup=%s duration=%s consistency=%s\n",
		cfg.Load.Endpoint, cfg.Load.Concurrency, cfg.Load.Rate, cfg.Load.Warmup, cfg.Load.Duration, cfg.Load.Consistency)
	var scraper *MetricsScraper
	if cfg.Metrics.PrometheusURL != "" {
		scraper = NewMetricsScraper(cfg.Metrics.PrometheusURL)
	}
	seedDur, _ := time.ParseDuration(st.SeedDuration)
	var report *Report
	if len(cfg.Load.Sweep.Rates) > 0 {
		results, err := RunSweep(client, corpus, cfg, scraper)
		if err != nil {
			return err
		}
		report = BuildSweepReport(results, corpus, cfg, st.TupleCount, seedDur)
		if report.SweepKneeRate > 0 {
			fmt.Printf("sweep knee: %d req/s\n", report.SweepKneeRate)
		} else {
			fmt.Println("sweep knee: none (every step saturated)")
		}
	} else {
		res, err := RunLoad(client, corpus, cfg, scraper)
		if err != nil {
			return err
		}
		report = BuildReport(res, corpus, cfg, st.TupleCount, seedDur)
	}
	jsonPath, mdPath, err := report.Save(cfg.OutputDir)
	if err != nil {
		return err
	}
	fmt.Printf("throughput: %.0f %s/sec | p50 %sms p95 %sms p99 %sms | errors %d | mismatches %d\n",
		report.Throughput, endpointNoun(cfg.Load.Endpoint), ms(report.Overall.P50), ms(report.Overall.P95), ms(report.Overall.P99),
		report.Overall.Errors, report.Mismatches)
	if report.ResultCounts != nil {
		fmt.Printf("result-set size: mean %.1f | p50 %d | p99 %d | max %d\n",
			report.ResultCounts.Mean, report.ResultCounts.P50, report.ResultCounts.P99, report.ResultCounts.Max)
	}
	if report.OfferedRate > 0 {
		fmt.Printf("achieved rate: %.0f req/s of %d offered (%d slots dropped) | response-latency p99 %sms\n",
			report.AchievedRate, report.OfferedRate, report.DroppedSlots, ms(report.ResponseLatency.P99))
	}
	if report.WriteChurn != nil {
		fmt.Printf("churn: %d writes/sec offered | %d write/delete calls | write p99 %sms | errors %d\n",
			report.WriteRate, report.WriteChurn.Count+report.WriteChurn.Errors, ms(report.WriteChurn.P99), report.WriteChurn.Errors)
	}
	if report.Server != nil && report.Server.DatastoreQueryCount.Count > 0 {
		fmt.Printf("server: %.2f datastore queries/request | server-side p99 %.2fms\n",
			report.Server.DatastoreQueryCount.Mean, report.Server.RequestDuration.P99)
	}
	fmt.Printf("wrote %s and %s\n", jsonPath, mdPath)
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loadState(cfg *Config) *State {
	data, err := os.ReadFile(cfg.StateFile)
	check(err)
	var st State
	check(json.Unmarshal(data, &st))
	return &st
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func runGenConfig(args []string) {
	fs := flag.NewFlagSet("gen-config", flag.ExitOnError)
	modelPath := fs.String("model", "", "path to compiled OpenFGA model JSON (required)")
	outPath := fs.String("o", "", "path to write generated YAML (defaults to stdout)")
	force := fs.Bool("force", false, "overwrite -o path if it already exists")
	fs.Parse(args)
	if *modelPath == "" {
		fail("gen-config: -model is required\n\nusage: fgaperf gen-config -model model.json [-o config.yaml] [-force]")
	}
	a, err := LoadModel(*modelPath)
	if err != nil {
		fail("%v", err)
	}
	var w io.Writer = os.Stdout
	if *outPath != "" {
		if _, err := os.Stat(*outPath); err == nil && !*force {
			fail("gen-config: %s already exists (pass -force to overwrite)", *outPath)
		}
		f, err := os.Create(*outPath)
		if err != nil {
			fail("%v", err)
		}
		defer f.Close()
		w = f
	}
	if err := generateConfig(*modelPath, a, w); err != nil {
		fail("%v", err)
	}
	if *outPath != "" {
		fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
	}
}
