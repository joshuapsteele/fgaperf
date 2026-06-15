package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveBaselineFromReport(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "results.json")
	data, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	baselinePath, err := saveBaselineAt(reportPath, dir, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(baselinePath) != "baseline-20260102-150405.json" {
		t.Fatalf("baseline path = %s", baselinePath)
	}
	b, err := LoadBaseline(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != baselineSchemaVersion {
		t.Errorf("schema = %d, want %d", b.SchemaVersion, baselineSchemaVersion)
	}
	if b.RandomSeed != 1 {
		t.Errorf("random_seed = %d, want 1", b.RandomSeed)
	}
	if b.ConfigFingerprint == "" {
		t.Fatal("config fingerprint is empty")
	}
	if b.Overall.P99 != 8*time.Millisecond {
		t.Errorf("baseline p99 = %v, want 8ms", b.Overall.P99)
	}
	if b.ByTarget["document#viewer"].P99 != 8*time.Millisecond {
		t.Errorf("target p99 = %v, want 8ms", b.ByTarget["document#viewer"].P99)
	}
}

func TestBaselineRegressionEvaluation(t *testing.T) {
	baseReport := sampleReport()
	base := baselineFromReport("baseline.json", baseReport, time.Now())

	t.Run("fails on p99 regression", func(t *testing.T) {
		current := sampleReport()
		current.Overall.P99 = 12 * time.Millisecond
		current.ByTarget["document#viewer"] = Stats{Count: 50, P50: 2 * time.Millisecond, P99: 12 * time.Millisecond}
		thresholds, err := parseMaxRegressions("p99=10%,throughput=-5%")
		if err != nil {
			t.Fatal(err)
		}
		cmp := evaluateBaseline(base, current, thresholds)
		got := strings.Join(cmp.failureStrings(), "\n")
		if !strings.Contains(got, "p99 overall") || !strings.Contains(got, "document#viewer") {
			t.Fatalf("failures did not name p99 overall and target:\n%s", got)
		}
	})

	t.Run("passes in tolerance", func(t *testing.T) {
		current := sampleReport()
		current.Overall.P99 = 8500 * time.Microsecond
		current.ByTarget["document#viewer"] = Stats{Count: 50, P50: 2 * time.Millisecond, P99: 8500 * time.Microsecond}
		current.Throughput = 4800 // above 5% drop limit from 5000
		thresholds, err := parseMaxRegressions("p99=10%,throughput=-5%")
		if err != nil {
			t.Fatal(err)
		}
		cmp := evaluateBaseline(base, current, thresholds)
		if len(cmp.Failures) != 0 {
			t.Fatalf("unexpected failures: %v", cmp.failureStrings())
		}
	})

	t.Run("fails when current throughput is zero", func(t *testing.T) {
		current := sampleReport()
		current.Throughput = 0
		current.Overall = Stats{Errors: 100}
		current.ByTarget["document#viewer"] = Stats{Errors: 50}
		thresholds, err := parseMaxRegressions("p99=10%,throughput=-5%")
		if err != nil {
			t.Fatal(err)
		}
		cmp := evaluateBaseline(base, current, thresholds)
		got := strings.Join(cmp.failureStrings(), "\n")
		if !strings.Contains(got, "throughput overall") {
			t.Fatalf("zero-throughput current run did not fail throughput gate; failures=%v warnings=%v", cmp.failureStrings(), cmp.Warnings)
		}
	})

	t.Run("config and shape differences are warnings", func(t *testing.T) {
		current := sampleReport()
		current.Duration = "30s"
		current.ResolvedConfig["random_seed"] = 2
		thresholds, err := parseMaxRegressions("p99=10%")
		if err != nil {
			t.Fatal(err)
		}
		cmp := evaluateBaseline(base, current, thresholds)
		if len(cmp.Failures) != 0 {
			t.Fatalf("warnings should not fail the gate: %v", cmp.failureStrings())
		}
		warnings := strings.Join(cmp.Warnings, "\n")
		for _, want := range []string{"measured duration differs", "resolved config fingerprint differs", "random_seed differs"} {
			if !strings.Contains(warnings, want) {
				t.Fatalf("warnings missing %q:\n%s", want, warnings)
			}
		}
	})
}

func TestCompareAgainstBaselineArtifactsAndExit(t *testing.T) {
	dir := t.TempDir()
	baseReport := sampleReport()
	baseData, err := json.Marshal(baselineFromReport("baseline-source.json", baseReport, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(baselinePath, baseData, 0o644); err != nil {
		t.Fatal(err)
	}
	current := sampleReport()
	current.Overall.P99 = 12 * time.Millisecond
	current.ByTarget["document#viewer"] = Stats{Count: 50, P50: 2 * time.Millisecond, P99: 12 * time.Millisecond}
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(dir, "current.json")
	if err := os.WriteFile(currentPath, currentData, 0o644); err != nil {
		t.Fatal(err)
	}

	generatedAt := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	err = compareAgainstBaselineAt(baselinePath, currentPath, dir, "p99=10%", true, generatedAt)
	if err == nil {
		t.Fatal("regressed current results passed baseline gate")
	}
	if !strings.Contains(err.Error(), "p99 overall") {
		t.Fatalf("error did not name regressed metric: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "baseline-compare-20260102-150405.md")); err != nil {
		t.Fatalf("comparison artifact missing: %v", err)
	}

	// Advisory mode: the same breaching results must not fail the gate, but the
	// comparison artifact is still written.
	advisoryAt := time.Date(2026, 1, 2, 15, 4, 6, 0, time.UTC)
	if err := compareAgainstBaselineAt(baselinePath, currentPath, dir, "p99=10%", false, advisoryAt); err != nil {
		t.Fatalf("advisory comparison failed the gate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "baseline-compare-20260102-150406.md")); err != nil {
		t.Fatalf("advisory comparison artifact missing: %v", err)
	}

	current.Overall.P99 = 8 * time.Millisecond
	current.ByTarget["document#viewer"] = Stats{Count: 50, P50: 2 * time.Millisecond, P99: 8 * time.Millisecond}
	currentData, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	okPath := filepath.Join(dir, "current-ok.json")
	if err := os.WriteFile(okPath, currentData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareAgainstBaselineAt(baselinePath, okPath, dir, "p99=10%", true, generatedAt); err != nil {
		t.Fatalf("in-tolerance current results failed baseline gate: %v", err)
	}
}

func TestParseMaxRegressions(t *testing.T) {
	th, err := parseMaxRegressions("p99=10%, throughput=-5%, datastore-queries/request=15%")
	if err != nil {
		t.Fatal(err)
	}
	if th["p99"] != 10 || th["throughput"] != -5 || th["ds_queries"] != 15 {
		t.Fatalf("thresholds parsed incorrectly: %#v", th)
	}
	if _, err := parseMaxRegressions("p99=ten"); err == nil {
		t.Fatal("invalid threshold passed")
	}
	if _, err := parseMaxRegressions("unknown=1%"); err == nil {
		t.Fatal("unknown metric passed")
	}
	for _, raw := range []string{"p99=NaN%", "p99=Inf%", "throughput=-Inf%"} {
		if _, err := parseMaxRegressions(raw); err == nil {
			t.Fatalf("non-finite threshold %q passed", raw)
		}
	}
}
