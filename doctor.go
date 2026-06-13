package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func doctor(cfg *Config) error {
	fmt.Printf("doctor for %s\n\n", cfg.OpenFGA.APIURL)
	failures := 0

	a, err := LoadModel(cfg.ModelFile)
	if err != nil {
		doctorLine("FAIL", "model file parses", modelLoadError(cfg.ModelFile, err))
		failures++
	} else if err := cfg.validateAgainstModel(a); err != nil {
		doctorLine("FAIL", "config matches model", err.Error())
		failures++
	} else {
		doctorLine("PASS", "model file parses", cfg.ModelFile)
		doctorLine("PASS", "config matches model", fmt.Sprintf("%d relations, %d subject types", len(a.AllRelations), len(a.SubjectTypes)))
	}

	client := NewFGAClient(cfg.OpenFGA, cfg.Load.Concurrency)
	if _, err := client.ListStores(); err != nil {
		doctorLine("FAIL", "OpenFGA API reachable", friendlyError(err, cfg))
		failures++
		doctorLine("SKIP", "store create/delete", "API is not reachable")
		doctorLine("SKIP", "write model", "API is not reachable")
	} else {
		doctorLine("PASS", "OpenFGA API reachable", cfg.OpenFGA.APIURL)
		storeName := fmt.Sprintf("%s-doctor-%d", cfg.OpenFGA.StoreName, time.Now().UnixNano())
		storeID, err := client.CreateStore(storeName)
		if err != nil {
			doctorLine("FAIL", "store create/delete", friendlyError(err, cfg))
			failures++
			doctorLine("SKIP", "write model", "temporary store was not created")
		} else {
			deleted := false
			defer func() {
				if !deleted {
					if err := client.DeleteStore(storeID); err != nil {
						fmt.Fprintf(os.Stderr, "doctor cleanup warning: deleting temp store %s failed: %v\n", storeID, err)
					}
				}
			}()
			doctorLine("PASS", "store create", storeID)
			if a == nil {
				doctorLine("SKIP", "write model", "model did not parse")
			} else if _, err := client.WriteModel(storeID, a.RawModel); err != nil {
				doctorLine("FAIL", "write model", friendlyError(err, cfg))
				failures++
			} else {
				doctorLine("PASS", "write model", "server accepted the compiled model JSON")
			}
			if err := client.DeleteStore(storeID); err != nil {
				doctorLine("FAIL", "store delete", friendlyError(err, cfg))
				failures++
			} else {
				deleted = true
				doctorLine("PASS", "store delete", "temporary store removed")
			}
		}
	}

	if cfg.Metrics.PrometheusURL == "" {
		doctorLine("SKIP", "Prometheus metrics", "metrics.prometheus_url is not set")
	} else if err := doctorMetrics(cfg.Metrics.PrometheusURL); err != nil {
		doctorLine("FAIL", "Prometheus metrics", err.Error())
		failures++
	} else {
		doctorLine("PASS", "Prometheus metrics", cfg.Metrics.PrometheusURL)
	}

	if failures > 0 {
		return fmt.Errorf("doctor found %d failing check(s)", failures)
	}
	fmt.Println("")
	fmt.Println(boldOut("doctor passed"))
	return nil
}

func doctorMetrics(url string) error {
	s, err := NewMetricsScraper(url).Snapshot()
	if err != nil {
		return fmt.Errorf("metrics endpoint not reachable at %s: %w", url, err)
	}
	var missing []string
	for _, fam := range []string{"openfga_request_duration_ms", "openfga_datastore_query_count"} {
		if s.Histograms[fam] == nil {
			missing = append(missing, fam)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("metrics endpoint reachable, but missing required families: %s", strings.Join(missing, ", "))
	}
	return nil
}

func doctorLine(status, name, detail string) {
	label := status
	switch status {
	case "PASS":
		label = styleFor(os.Stdout, "32", "PASS")
	case "FAIL":
		label = styleFor(os.Stdout, "31", "FAIL")
	case "WARN":
		label = styleFor(os.Stdout, "33", "WARN")
	case "SKIP":
		label = styleFor(os.Stdout, "2", "SKIP")
	}
	if detail == "" {
		fmt.Printf("[%s] %s\n", label, name)
		return
	}
	detail = strings.ReplaceAll(detail, "\n", "\n       ")
	fmt.Printf("[%s] %s: %s\n", label, name, detail)
}
