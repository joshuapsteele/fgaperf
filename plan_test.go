package main

import (
	"strings"
	"testing"
)

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

func TestPlanReportIncludesResolvedConfigAndWarnings(t *testing.T) {
	a := &Analysis{
		Model: &Model{SchemaVersion: "1.1"},
		Types: []string{"doc"},
		TypeDefs: map[string]*TypeDefinition{
			"doc": {Type: "doc", Relations: map[string]Userset{
				"computed": {ComputedUserset: &ObjectRelation{Relation: "missing"}},
			}},
		},
		DirectRefs:   map[string]map[string][]RelationReference{"doc": {}},
		AllRelations: []TypeRelation{{Type: "doc", Relation: "computed"}},
		Conditioned:  map[string]bool{},
	}
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModelFile = "mini.json"
	cfg.Probe.Targets = []TargetSpec{{Relation: "doc#computed", Weight: 1}}

	var b strings.Builder
	planReport(a, cfg, &b)
	got := b.String()
	for _, want := range []string{
		"resolved config (credentials redacted):",
		"load phase time budget:",
		"- probe target doc#computed has no reachable direct tuple path",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan output missing %q:\n%s", want, got)
		}
	}
}
