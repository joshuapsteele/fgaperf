package main

// fgaperf: model-driven performance testing for OpenFGA.
//
//	fgaperf setup -config config.yaml   create store, write model, seed tuples
//	fgaperf probe -config config.yaml   build the check corpus
//	fgaperf run   -config config.yaml   run the load test, write report
//	fgaperf all   -config config.yaml   all of the above, then delete the store
//	fgaperf inspect -config config.yaml print the model analysis and exit
//	fgaperf plan  -config config.yaml   preview seeded tuple counts; no server
//	fgaperf cleanup -config config.yaml delete the store recorded in the state file
//	fgaperf compare a.json b.json       render two results files side by side
//	fgaperf baseline save results.json  save compact regression baseline
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
	BatchSize    int       `json:"batch_size"`    // seed.batch_size at seed time; resume skips committed batches by their original boundaries, so it must not change
	SeededTuples int       `json:"seeded_tuples"` // high-water mark for resume; == TupleCount when complete
	SeedComplete bool      `json:"seed_complete"`
	SeedDuration string    `json:"seed_duration"`
	SeededAt     time.Time `json:"seeded_at"`
}

func saveState(cfg *Config, st *State) error {
	data, _ := json.MarshalIndent(st, "", " ")
	return os.WriteFile(cfg.StateFile, data, 0o644)
}

func validateResumeState(st State, tupleCount, batchSize int) error {
	if st.TupleCount != tupleCount {
		return fmt.Errorf("cannot resume: the seed changed since it was interrupted (was %d tuples, now %d) — `fgaperf cleanup` then `setup` fresh", st.TupleCount, tupleCount)
	}
	// Resume re-batches tuples[startIndex:] into seed.batch_size chunks and skips
	// any batch the server rejects as already-committed. That skip is only safe
	// while batch boundaries are identical across runs; a changed batch_size lets
	// a new batch straddle an old boundary, and OpenFGA rejects the whole
	// (transactional) batch wholesale — dropping its not-yet-written tuples with
	// it and marking the seed complete with a hole. A recorded size of 0 is a
	// pre-this-field state file; tolerate it rather than block a legacy resume.
	if st.BatchSize != 0 && st.BatchSize != batchSize {
		return fmt.Errorf("cannot resume: seed.batch_size changed since the seed was interrupted (was %d, now %d); resume relies on identical batch boundaries — `fgaperf cleanup` then `setup` fresh", st.BatchSize, batchSize)
	}
	if st.SeededTuples < 0 || st.SeededTuples > st.TupleCount {
		return fmt.Errorf("cannot resume: state seeded_tuples %d is outside 0..%d", st.SeededTuples, st.TupleCount)
	}
	return nil
}

func validateStateCorpus(st *State, corpus *Corpus) error {
	if st == nil || corpus == nil {
		return nil
	}
	if st.StoreID != "" && corpus.StoreID != "" && st.StoreID != corpus.StoreID {
		return fmt.Errorf("state/corpus mismatch: state store_id %q does not match corpus store_id %q; run setup and probe for the same store", st.StoreID, corpus.StoreID)
	}
	if st.ModelID != "" && corpus.ModelID != "" && st.ModelID != corpus.ModelID {
		return fmt.Errorf("state/corpus mismatch: state model_id %q does not match corpus model_id %q; run setup and probe for the same model", st.ModelID, corpus.ModelID)
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printRootHelp(os.Stderr)
		os.Exit(1)
	}
	cmd := os.Args[1]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		printRootHelp(os.Stdout)
		return
	}
	// gen-config is the one command that takes neither -config nor a loaded
	// model from the configured path; it operates directly on a model file
	// and writes annotated YAML to stdout (or -o path). Handled inline so the
	// shared flag block below doesn't need to learn its quirks.
	if cmd == "gen-config" {
		runGenConfig(os.Args[2:])
		return
	}
	if _, ok := commandDocs[cmd]; !ok {
		printRootHelp(os.Stderr)
		fail("unknown command %q", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { printCommandHelp(os.Stdout, cmd, fs) }
	cfgPath := fs.String("config", "config.yaml", "path to config file (optional; defaults apply)")
	keep := fs.Bool("keep", false, "all: keep the store and state file instead of deleting them")
	allStores := fs.Bool("all-stores", false, "cleanup: delete every store whose name matches openfga.store_name")
	resume := fs.Bool("resume", false, "setup: resume an interrupted seed recorded in the state file")
	inspectJSON := fs.Bool("json", false, "inspect: print model analysis as JSON")
	// Common load knobs as flags so a quick run doesn't need a config edit (or
	// the sed dance the README used to recommend). Only flags actually passed
	// override the config; defaults here are inert sentinels.
	durFlag := fs.Duration("duration", 0, "override load.duration")
	warmupFlag := fs.Duration("warmup", 0, "override load.warmup")
	rateFlag := fs.Int("rate", 0, "override load.rate (req/s; 0 = closed loop)")
	concFlag := fs.Int("concurrency", 0, "override load.concurrency")
	endpointFlag := fs.String("endpoint", "", "override load.endpoint (check|batch-check|list-objects|list-users)")
	consistencyFlag := fs.String("consistency", "", "override load.consistency (MINIMIZE_LATENCY|HIGHER_CONSISTENCY)")
	transportFlag := fs.String("transport", "", "override load.transport (http|grpc)")
	outDirFlag := fs.String("output-dir", "", "override output_dir")
	againstBaseline := fs.String("against-baseline", "", "compare: compare one results JSON against a saved baseline")
	maxRegression := fs.String("max-regression", defaultMaxRegression, "compare -against-baseline: comma-separated thresholds, e.g. p99=10%,throughput=-5%")
	fs.Parse(os.Args[2:])

	cfgFile := *cfgPath
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "config %s not found, using built-in defaults\n", cfgFile)
		cfgFile = ""
	}
	cfg, err := LoadConfigFile(cfgFile)
	if err != nil {
		fail("%v", err)
	}

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
		case "transport":
			ov.Transport = transportFlag
		case "output-dir":
			ov.OutputDir = outDirFlag
		}
	})
	if err := cfg.applyOverrides(ov); err != nil {
		fail("invalid flag override: %v", err)
	}

	if cmd == "doctor" {
		checkWithConfig(doctor(cfg), cfg)
		return
	}

	// run and cleanup operate on an existing store and never read the model.
	var analysis *Analysis
	switch cmd {
	case "inspect", "setup", "probe", "all", "plan", "validate":
		analysis, err = LoadModel(cfg.ModelFile)
		if err != nil {
			fail("%s", modelLoadError(cfg.ModelFile, err))
		}
		if err := cfg.validateAgainstModel(analysis); err != nil {
			fail("invalid config: %v", err)
		}
	}

	newClient := func() *FGAClient {
		return NewFGAClient(cfg.OpenFGA, cfg.Load.Concurrency)
	}

	switch cmd {
	case "inspect":
		if *inspectJSON {
			check(json.NewEncoder(os.Stdout).Encode(inspectProjection(analysis, cfg)))
		} else {
			inspect(analysis, cfg)
		}
	case "plan":
		plan(analysis, cfg)
	case "validate":
		validateOnly(analysis, cfg)
	case "setup":
		_, err := setup(newClient(), analysis, cfg, *resume)
		checkWithConfig(err, cfg)
	case "probe":
		st, err := loadState(cfg)
		checkWithConfig(err, cfg)
		checkWithConfig(probe(newClient(), analysis, cfg, st), cfg)
	case "run":
		st, err := loadState(cfg)
		checkWithConfig(err, cfg)
		checkWithConfig(run(newClient(), cfg, st), cfg)
	case "all":
		checkWithConfig(runAll(newClient(), analysis, cfg, *keep || cfg.KeepStore), cfg)
	case "cleanup":
		checkWithConfig(cleanup(newClient(), cfg, *allStores), cfg)
	case "baseline":
		args := fs.Args()
		if len(args) < 1 || args[0] != "save" {
			fail("usage: fgaperf baseline save <results.json>")
		}
		saveFS := flag.NewFlagSet("baseline save", flag.ExitOnError)
		saveOutDir := saveFS.String("output-dir", cfg.OutputDir, "override output_dir")
		saveFS.Parse(args[1:])
		if saveFS.NArg() != 1 {
			fail("usage: fgaperf baseline save [-output-dir dir] <results.json>")
		}
		checkWithConfig(saveBaseline(saveFS.Arg(0), *saveOutDir), cfg)
	case "compare":
		args := fs.Args()
		if *againstBaseline != "" {
			if len(args) != 1 {
				fail("usage: fgaperf compare -against-baseline <baseline.json> [flags] <results.json>")
			}
			checkWithConfig(compareAgainstBaseline(*againstBaseline, args[0], cfg.OutputDir, *maxRegression), cfg)
			return
		}
		if len(args) != 2 {
			fail("usage: fgaperf compare <results-a.json> <results-b.json>")
		}
		checkWithConfig(compare(args[0], args[1], cfg.OutputDir), cfg)
	default:
		fail("unknown command %q", cmd)
	}
}

// runAll executes setup, probe, and run, then deletes the store it created so
// repeated runs against a deployed OpenFGA leave nothing behind. The store is
// also deleted when a phase fails or the process is interrupted.
func runAll(client *FGAClient, a *Analysis, cfg *Config, keep bool) error {
	st, err := setup(client, a, cfg, false)
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

func setup(client *FGAClient, a *Analysis, cfg *Config, resume bool) (*State, error) {
	// Generate first: deterministic, and resume needs the same tuple order to
	// know the prefix it already wrote is still the prefix it would write now.
	world := NewWorld(a, cfg)
	tuples := world.GenerateTuples()

	var st *State
	startIndex := 0
	resuming := false
	if resume {
		if data, err := os.ReadFile(cfg.StateFile); err == nil {
			var prev State
			if json.Unmarshal(data, &prev) == nil && prev.StoreID != "" && !prev.SeedComplete {
				if err := validateResumeState(prev, len(tuples), cfg.Seed.BatchSize); err != nil {
					return nil, err
				}
				st = &prev
				startIndex = prev.SeededTuples
				resuming = true
				fmt.Printf("resuming seed of store %s from tuple %d/%d\n", st.StoreID, startIndex, len(tuples))
			}
		}
		if st == nil {
			fmt.Fprintln(os.Stderr, "no resumable seed in the state file; starting fresh")
		}
	}

	if st == nil {
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
		fmt.Printf("generated %d tuples across %d cohorts\n", len(tuples), cfg.Seed.Cohorts)
		st = &State{StoreID: storeID, ModelID: modelID, TupleCount: len(tuples), BatchSize: cfg.Seed.BatchSize}
		// Persist a partial state up front so an interrupted seed is resumable.
		if err := saveState(cfg, st); err != nil {
			return nil, err
		}
	}

	checkpoint := func(written int) {
		st.SeededTuples = written
		saveState(cfg, st) // best-effort mid-seed; the final save below is authoritative
	}
	dur, err := SeedStore(client, st.StoreID, st.ModelID, tuples, cfg, startIndex, resuming, checkpoint)
	if err != nil {
		saveState(cfg, st) // persist the last clean prefix so `setup -resume` can continue
		return nil, fmt.Errorf("seeding: %w (resume with `fgaperf setup -resume`)", err)
	}
	seeded := len(tuples) - startIndex
	fmt.Printf("seeded %d tuples in %s (%.0f tuples/sec)\n", seeded, dur.Round(time.Millisecond), float64(seeded)/dur.Seconds())

	st.SeededTuples = len(tuples)
	st.SeedComplete = true
	st.SeedDuration = dur.String()
	st.SeededAt = time.Now().UTC()
	if err := saveState(cfg, st); err != nil {
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
			fmt.Fprintln(os.Stderr, yellowErr(fmt.Sprintf("probe: target %s corpus is %d entries but only %d distinct checks (%.1fx duplication); cache hit rates under load will be inflated",
				target, st.Total, st.Distinct, float64(st.Total)/float64(st.Distinct))))
		}
	}
	return corpus.Save(cfg.CorpusFile)
}

func run(client *FGAClient, cfg *Config, st *State) error {
	corpus, err := LoadCorpus(cfg.CorpusFile)
	if err != nil {
		return fmt.Errorf("loading corpus (run probe first?): %w", err)
	}
	if err := validateStateCorpus(st, corpus); err != nil {
		return err
	}
	fmt.Printf("load: endpoint=%s transport=%s concurrency=%d rate=%v warmup=%s duration=%s consistency=%s\n",
		cfg.Load.Endpoint, cfg.Load.Transport, cfg.Load.Concurrency, cfg.Load.Rate, cfg.Load.Warmup, cfg.Load.Duration, cfg.Load.Consistency)
	// The measured phase runs over the configured transport; setup and probe
	// already ran over HTTP on `client`. For gRPC we open a dedicated connection
	// here and close it when the run finishes.
	var loadClient LoadClient = client
	if cfg.Load.Transport == "grpc" {
		gc, err := NewGRPCClient(cfg.OpenFGA)
		if err != nil {
			return fmt.Errorf("gRPC transport: %w", err)
		}
		defer gc.Close()
		fmt.Printf("load transport: gRPC -> %s\n", grpcAddr(cfg.OpenFGA))
		loadClient = gc
	}
	var scraper *MetricsScraper
	if cfg.Metrics.PrometheusURL != "" {
		scraper = NewMetricsScraper(cfg.Metrics.PrometheusURL)
	}
	seedDur, _ := time.ParseDuration(st.SeedDuration)
	var report *Report
	if len(cfg.Load.Sweep.Rates) > 0 {
		results, err := RunSweep(loadClient, corpus, cfg, scraper)
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
		res, err := RunLoad(loadClient, corpus, cfg, scraper)
		if err != nil {
			return err
		}
		report = BuildReport(res, corpus, cfg, st.TupleCount, seedDur)
	}
	jsonPath, mdPath, err := report.Save(cfg.OutputDir)
	if err != nil {
		return err
	}
	fmt.Printf("%s %.0f %s/sec | p50 %sms p95 %sms p99 %sms | errors %d | mismatches %d\n",
		boldOut("throughput:"), report.Throughput, endpointNoun(cfg.Load.Endpoint), ms(report.Overall.P50), ms(report.Overall.P95), ms(report.Overall.P99),
		report.Overall.Errors, report.Mismatches)
	if report.ResultCounts != nil {
		fmt.Printf("%s mean %.1f | p50 %d | p99 %d | max %d\n",
			boldOut("result-set size:"), report.ResultCounts.Mean, report.ResultCounts.P50, report.ResultCounts.P99, report.ResultCounts.Max)
	}
	if report.OfferedRate > 0 {
		fmt.Printf("%s %.0f req/s of %d offered (%d slots dropped) | response-latency p99 %sms\n",
			boldOut("achieved rate:"), report.AchievedRate, report.OfferedRate, report.DroppedSlots, ms(report.ResponseLatency.P99))
	}
	if report.WriteChurn != nil {
		fmt.Printf("%s %d writes/sec offered | %d write/delete calls | write p99 %sms | errors %d\n",
			boldOut("churn:"), report.WriteRate, report.WriteChurn.Count+report.WriteChurn.Errors, ms(report.WriteChurn.P99), report.WriteChurn.Errors)
	}
	if report.Server != nil && report.Server.DatastoreQueryCount.Count > 0 {
		fmt.Printf("%s %.2f datastore queries/request | server-side p99 %.2fms\n",
			boldOut("server:"), report.Server.DatastoreQueryCount.Mean, report.Server.RequestDuration.P99)
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

func loadState(cfg *Config) (*State, error) {
	data, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		return nil, fmt.Errorf("reading state file %s (run `fgaperf setup` first, or `fgaperf all` for the full workflow): %w", cfg.StateFile, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing state file %s: %w", cfg.StateFile, err)
	}
	return &st, nil
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func checkWithConfig(err error, cfg *Config) {
	if err != nil {
		fail("%s", friendlyError(err, cfg))
	}
}

func fail(format string, args ...any) {
	fmt.Fprintln(os.Stderr, redErr(fmt.Sprintf(format, args...)))
	os.Exit(1)
}

func runGenConfig(args []string) {
	fs := flag.NewFlagSet("gen-config", flag.ExitOnError)
	fs.Usage = func() { printCommandHelp(os.Stdout, "gen-config", fs) }
	modelPath := fs.String("model", "", "path to compiled OpenFGA model JSON (required)")
	outPath := fs.String("o", "", "path to write generated YAML (defaults to stdout)")
	force := fs.Bool("force", false, "overwrite -o path if it already exists")
	fs.Parse(args)
	if *modelPath == "" {
		fail("gen-config: -model is required\n\nusage: fgaperf gen-config -model model.json [-o config.yaml] [-force]")
	}
	a, err := LoadModel(*modelPath)
	if err != nil {
		fail("%s", modelLoadError(*modelPath, err))
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
