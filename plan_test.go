package main

import "testing"

// planTupleCounts must mirror GenerateTuples' fanout logic from config alone.
func TestPlanTupleCounts(t *testing.T) {
	a := loadExampleModel(t)
	cfg, err := LoadConfigFile("") // defaults: 25 instances/type, fanout 2, wildcard_prob 1.0
	if err != nil {
		t.Fatal(err)
	}

	inst := planInstanceCounts(a, cfg)
	if inst["document"] != 25 || inst["user"] != 25 {
		t.Errorf("instance counts: %+v, want 25 each", inst)
	}

	counts := planTupleCounts(a, cfg)
	// document#viewer refs: {user} fanout 2 (=50), {user,wildcard} prob 1.0 (=25),
	// {group#member} fanout 2 (=50) => 125.
	if got := counts["document#viewer"]; got != 125 {
		t.Errorf("document#viewer = %d, want 125", got)
	}

	// A contextual relation's tuples ride on requests, not the store, so it is
	// excluded from the seeded-tuple estimate.
	cfg.Contextual.Relations = []string{"document#active_context"}
	counts = planTupleCounts(a, cfg)
	if got, ok := counts["document#active_context"]; ok && got != 0 {
		t.Errorf("contextual relation should be excluded, got %d", got)
	}
}
