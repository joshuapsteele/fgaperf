package main

import (
	"reflect"
	"testing"
)

// The example model is the test fixture: it exercises usersets, tuple-to-
// userset, a recursive relation, an intersection, and a conditioned wildcard.
func loadExampleModel(t *testing.T) *Analysis {
	t.Helper()
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatalf("loading example model: %v", err)
	}
	return a
}

func TestExampleModelAnalysis(t *testing.T) {
	a := loadExampleModel(t)

	if got := len(a.Types); got != 4 {
		t.Errorf("types: got %d, want 4", got)
	}
	if got := len(a.AllRelations); got != 9 {
		t.Errorf("relations: got %d, want 9", got)
	}
	if !reflect.DeepEqual(a.SubjectTypes, []string{"user"}) {
		t.Errorf("subject types: got %v, want [user]", a.SubjectTypes)
	}
}

// The conditioned fixpoint must propagate the condition on document#viewer's
// wildcard through the rewrite graph: can_share intersects with viewer, so it
// is CEL-reachable; editor and the folder relations are not.
func TestConditionedFixpoint(t *testing.T) {
	a := loadExampleModel(t)

	want := map[string]bool{
		"document#viewer":    true,
		"document#can_share": true,
		"document#editor":    false,
		"document#owner":     false,
		"folder#viewer":      false,
		"folder#owner":       false,
		"group#member":       false,
	}
	for rel, cond := range want {
		if a.Conditioned[rel] != cond {
			t.Errorf("Conditioned[%s] = %v, want %v", rel, a.Conditioned[rel], cond)
		}
	}
}

func TestTupleContextParamsDefaultSplit(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")

	tupleSide, requestSide := a.TupleContextParams("has_scope", cfg)
	if !reflect.DeepEqual(tupleSide, []string{"granted_scopes"}) {
		t.Errorf("tuple-side: got %v, want [granted_scopes] (maps ride on the tuple)", tupleSide)
	}
	if !reflect.DeepEqual(requestSide, []string{"required_scope"}) {
		t.Errorf("request-side: got %v, want [required_scope] (scalars come from the request)", requestSide)
	}
}

func TestTupleContextParamsOverride(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	cfg.Conditions = map[string]CondConfig{
		"has_scope": {TupleParams: []string{"required_scope"}},
	}

	tupleSide, requestSide := a.TupleContextParams("has_scope", cfg)
	if !reflect.DeepEqual(tupleSide, []string{"required_scope"}) {
		t.Errorf("tuple-side: got %v, want [required_scope] (config override)", tupleSide)
	}
	if !reflect.DeepEqual(requestSide, []string{"granted_scopes"}) {
		t.Errorf("request-side: got %v, want [granted_scopes]", requestSide)
	}
}
