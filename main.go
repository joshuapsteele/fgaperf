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

import (
	"encoding/json"
	"flag"
	"fmt"
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
		fail("usage: fgaperf <setup|probe|run|all|inspect|cleanup|compare> [-config config.yaml]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to config file (optional; defaults apply)")
	keep := fs.Bool("keep", false, "all: keep the store and state file instead of deleting them")
	allStores := fs.Bool("all-stores", false, "cleanup: delete every store whose name matches openfga.store_name")
	fs.Parse(os.Args[2:])

	cfgFile := *cfgPath
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "config %s not found, using built-in defaults\n", cfgFile)
		cfgFile = ""
	}
	cfg, err := LoadConfigFile(cfgFile)
	check(err)

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
	contextual := contextualSet(cfg)
	fmt.Println("\nrelations (— = unconditioned, CEL = condition reachable):")
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
	fmt.Println("\nconditions:")
	for name, c := range a.Model.Conditions {
		ts, rs := a.TupleContextParams(name, cfg)
		fmt.Printf("  %s: %q  tuple-side params: %v  request-side params: %v\n", name, c.Expression, ts, rs)
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
	fmt.Printf("throughput: %.0f checks/sec | p50 %sms p95 %sms p99 %sms | errors %d | mismatches %d\n",
		report.Throughput, ms(report.Overall.P50), ms(report.Overall.P95), ms(report.Overall.P99),
		report.Overall.Errors, report.Mismatches)
	if report.OfferedRate > 0 {
		fmt.Printf("achieved rate: %.0f req/s of %d offered (%d slots dropped) | response-latency p99 %sms\n",
			report.AchievedRate, report.OfferedRate, report.DroppedSlots, ms(report.ResponseLatency.P99))
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
