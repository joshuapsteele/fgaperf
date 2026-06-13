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

func TestPlanTupleCountsWildcardIsUpperBound(t *testing.T) {
	a := &Analysis{
		Types: []string{"document"},
		DirectRefs: map[string]map[string][]RelationReference{
			"document": {
				"viewer": {{Type: "user", Wildcard: &struct{}{}}},
			},
		},
	}
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Seed.DefaultCount = 1
	cfg.Seed.WildcardProb = 0.5

	counts := planTupleCounts(a, cfg)
	if got := counts["document#viewer"]; got != 1 {
		t.Errorf("wildcard upper bound = %d, want 1", got)
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

func TestPlanWarningsUsePositiveReachability(t *testing.T) {
	a := &Analysis{
		Model: &Model{SchemaVersion: "1.1"},
		Types: []string{"doc", "user"},
		TypeDefs: map[string]*TypeDefinition{
			"doc": {Type: "doc", Relations: map[string]Userset{
				"viewer":  {This: &struct{}{}},
				"blocked": {This: &struct{}{}},
				"active":  {},
				"owner":   {},
				"can_view": {Intersection: &Usersets{Child: []Userset{
					{ComputedUserset: &ObjectRelation{Relation: "viewer"}},
					{ComputedUserset: &ObjectRelation{Relation: "active"}},
				}}},
				"can_manage": {Difference: &Difference{
					Base:     Userset{ComputedUserset: &ObjectRelation{Relation: "owner"}},
					Subtract: Userset{ComputedUserset: &ObjectRelation{Relation: "blocked"}},
				}},
			}},
			"user": {Type: "user"},
		},
		DirectRefs: map[string]map[string][]RelationReference{
			"doc": {
				"viewer":  {{Type: "user"}},
				"blocked": {{Type: "user"}},
			},
			"user": {},
		},
		SubjectTypes: []string{"user"},
	}
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Probe.Targets = []TargetSpec{
		{Relation: "doc#can_view", Weight: 1},
		{Relation: "doc#can_manage", Weight: 1},
	}

	got := strings.Join(planWarnings(a, cfg), "\n")
	for _, want := range []string{"probe target doc#can_view", "probe target doc#can_manage"} {
		if !strings.Contains(got, want) {
			t.Fatalf("warnings missing %q:\n%s", want, got)
		}
	}
}

func TestPlanWarningsTupleToUsersetRequiresComputedPath(t *testing.T) {
	a := &Analysis{
		Model: &Model{SchemaVersion: "1.1"},
		Types: []string{"document", "folder", "user"},
		TypeDefs: map[string]*TypeDefinition{
			"document": {Type: "document", Relations: map[string]Userset{
				"parent": {This: &struct{}{}},
				"viewer": {TupleToUserset: &TupleToUserset{
					Tupleset:        ObjectRelation{Relation: "parent"},
					ComputedUserset: ObjectRelation{Relation: "viewer"},
				}},
			}},
			"folder": {Type: "folder", Relations: map[string]Userset{"viewer": {}}},
			"user":   {Type: "user"},
		},
		DirectRefs: map[string]map[string][]RelationReference{
			"document": {"parent": {{Type: "folder"}}},
			"folder":   {},
			"user":     {},
		},
		SubjectTypes: []string{"user"},
	}
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Probe.Targets = []TargetSpec{{Relation: "document#viewer", Weight: 1}}

	got := strings.Join(planWarnings(a, cfg), "\n")
	if !strings.Contains(got, "probe target document#viewer") {
		t.Fatalf("tuple-to-userset with no computed path did not warn:\n%s", got)
	}
}

func TestPlanWarningsZeroInstances(t *testing.T) {
	a := &Analysis{
		Model: &Model{SchemaVersion: "1.1"},
		Types: []string{"doc", "user"},
		TypeDefs: map[string]*TypeDefinition{
			"doc":  {Type: "doc", Relations: map[string]Userset{"viewer": {This: &struct{}{}}}},
			"user": {Type: "user"},
		},
		DirectRefs:   map[string]map[string][]RelationReference{"doc": {"viewer": {{Type: "user"}}}, "user": {}},
		SubjectTypes: []string{"user"},
	}
	cfg, err := LoadConfigFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Seed.Instances = map[string]int{"doc": 0}
	cfg.Probe.Targets = []TargetSpec{{Relation: "doc#viewer", Weight: 1}}

	got := strings.Join(planWarnings(a, cfg), "\n")
	if !strings.Contains(got, "zero configured doc instances") {
		t.Fatalf("zero-instance target did not warn:\n%s", got)
	}
}
