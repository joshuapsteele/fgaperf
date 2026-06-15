package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateStateCorpusConsistency(t *testing.T) {
	st := &State{StoreID: "state-store", ModelID: "model-1"}
	corpus := &Corpus{StoreID: "corpus-store", ModelID: "model-1"}
	if err := validateStateCorpus(st, corpus); err == nil {
		t.Fatal("state/corpus store mismatch passed validation")
	}

	corpus.StoreID = "state-store"
	corpus.ModelID = "model-2"
	if err := validateStateCorpus(st, corpus); err == nil {
		t.Fatal("state/corpus model mismatch passed validation")
	}

	corpus.ModelID = "model-1"
	if err := validateStateCorpus(st, corpus); err != nil {
		t.Fatalf("matching state/corpus rejected: %v", err)
	}
}

func TestValidateResumeStateBounds(t *testing.T) {
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: -1, BatchSize: 100}, 10, 100); err == nil {
		t.Fatal("negative seeded_tuples passed validation")
	}
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: 11, BatchSize: 100}, 10, 100); err == nil {
		t.Fatal("out-of-range seeded_tuples passed validation")
	}
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: 5, BatchSize: 100}, 10, 100); err != nil {
		t.Fatalf("valid resume state rejected: %v", err)
	}
}

// Resume skips already-committed batches by their original boundaries, so a
// changed seed.batch_size must be rejected: re-batching at a different size lets
// a new batch straddle an old boundary, and OpenFGA rejects the whole
// transactional batch as a duplicate — dropping its not-yet-written tuples and
// quietly marking the seed complete with a hole.
func TestValidateResumeStateBatchSize(t *testing.T) {
	base := State{TupleCount: 100, SeededTuples: 50, BatchSize: 100}
	if err := validateResumeState(base, 100, 100); err != nil {
		t.Fatalf("unchanged batch size rejected: %v", err)
	}
	if err := validateResumeState(base, 100, 50); err == nil {
		t.Fatal("changed batch size must be rejected (resume alignment depends on it)")
	}
	// A pre-this-field state file records BatchSize 0; tolerate it so a legacy
	// interrupted seed can still resume (no worse than before the field existed).
	legacy := State{TupleCount: 100, SeededTuples: 50, BatchSize: 0}
	if err := validateResumeState(legacy, 100, 50); err != nil {
		t.Fatalf("legacy state with no recorded batch size should resume: %v", err)
	}
	// A tuple-count mismatch is still caught even when the batch size matches.
	if err := validateResumeState(base, 99, 100); err == nil {
		t.Fatal("changed tuple count must be rejected")
	}
}

func TestProbeAndRunRejectIncompleteSeedState(t *testing.T) {
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	st := &State{StoreID: "store-1", ModelID: "model-1", TupleCount: 100, SeededTuples: 10, SeedComplete: false}
	for name, err := range map[string]error{
		"probe": probe(nil, nil, cfg, st),
		"run":   run(nil, cfg, st),
	} {
		if err == nil {
			t.Fatalf("%s accepted incomplete seed state", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "incomplete seed") || !strings.Contains(msg, "setup -resume") {
			t.Fatalf("%s returned non-actionable error: %v", name, err)
		}
	}

	st.SeededTuples = 100
	st.SeedComplete = true
	if err := validateReadyState(st); err != nil {
		t.Fatalf("complete seed state rejected: %v", err)
	}
}

func TestCLIHelperMain(t *testing.T) {
	if os.Getenv("FGAPERF_TEST_MAIN") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"fgaperf"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	fmt.Fprintln(os.Stderr, "missing helper argument separator")
	os.Exit(2)
}

func TestNoServerCommandsSkipOIDCTokenFetch(t *testing.T) {
	var tokenRequests int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"unexpected","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	modelPath, err := filepath.Abs("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`model_file: %q
output_dir: %q
openfga:
  timeout: 1s
  oidc:
    token_url: %q
    client_id: id
    client_secret: secret
`, modelPath, dir, tokenSrv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	reportData, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	reportA := filepath.Join(dir, "a.json")
	reportB := filepath.Join(dir, "b.json")
	if err := os.WriteFile(reportA, reportData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportB, reportData, 0o644); err != nil {
		t.Fatal(err)
	}
	baselineData, err := json.Marshal(baselineFromReport(reportA, sampleReport(), sampleReport().GeneratedAt))
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(baselinePath, baselineData, 0o644); err != nil {
		t.Fatal(err)
	}

	commands := [][]string{
		{"plan", "-config", cfgPath},
		{"validate", "-config", cfgPath},
		{"inspect", "-config", cfgPath},
		{"inspect", "-config", cfgPath, "-json"},
		{"baseline", "-config", cfgPath, "save", reportA},
		{"compare", "-config", cfgPath, reportA, reportB},
		{"compare", "-config", cfgPath, "-against-baseline", baselinePath, reportA},
		{"merge", "-config", cfgPath, reportA, reportB},
	}
	for _, args := range commands {
		runCLIHelper(t, args...)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 0 {
		t.Fatalf("no-server commands fetched OIDC token %d time(s)", got)
	}
}

func runCLIHelper(t *testing.T, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestCLIHelperMain", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "FGAPERF_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
}
