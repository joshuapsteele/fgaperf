package main

import (
	"reflect"
	"strings"
	"testing"
)

func exampleWorld(t *testing.T) (*World, []TupleKey) {
	t.Helper()
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	w := NewWorld(a, cfg)
	return w, w.GenerateTuples()
}

func TestGenerateTuplesDeterministic(t *testing.T) {
	_, first := exampleWorld(t)
	_, second := exampleWorld(t)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced different tuple graphs")
	}
}

func TestGenerateTuplesNoDuplicates(t *testing.T) {
	_, tuples := exampleWorld(t)
	seen := map[string]bool{}
	for _, tu := range tuples {
		key := tu.User + "|" + tu.Relation + "|" + tu.Object
		if seen[key] {
			t.Fatalf("duplicate tuple: %s", key)
		}
		seen[key] = true
	}
}

// Self-referential relations must only link from higher to lower instance
// indices so the generated graph is acyclic by construction.
func TestSelfReferentialTuplesAreAcyclic(t *testing.T) {
	_, tuples := exampleWorld(t)
	n := 0
	for _, tu := range tuples {
		if tu.Relation != "parent" || !strings.HasPrefix(tu.Object, "folder:") {
			continue
		}
		if !strings.HasPrefix(tu.User, "folder:") {
			t.Fatalf("folder#parent subject is not a folder: %s", tu.User)
		}
		if instanceIndex(tu.User) >= instanceIndex(tu.Object) {
			t.Errorf("cycle risk: %s -> parent -> %s does not point downward", tu.User, tu.Object)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no folder#parent tuples generated; test is vacuous")
	}
}

// Tuples linking instances must stay within a cohort; that is the property
// that makes intersections resolvable.
func TestTuplesLinkWithinCohorts(t *testing.T) {
	w, tuples := exampleWorld(t)
	for _, tu := range tuples {
		user := strings.TrimSuffix(tu.User, "#member")
		if strings.HasSuffix(user, ":*") {
			continue
		}
		uc, ok1 := w.Cohort[user]
		oc, ok2 := w.Cohort[tu.Object]
		if !ok1 || !ok2 {
			t.Fatalf("tuple references unknown instance: %s -> %s", tu.User, tu.Object)
		}
		if uc != oc {
			t.Errorf("cross-cohort tuple: %s (cohort %d) %s %s (cohort %d)", tu.User, uc, tu.Relation, tu.Object, oc)
		}
	}
}

// Conditioned wildcard tuples must carry the condition name and the
// tuple-side context (the granted_scopes map), or the server rejects them.
func TestConditionedWildcardTuplesCarryContext(t *testing.T) {
	_, tuples := exampleWorld(t)
	n := 0
	for _, tu := range tuples {
		if tu.User != "user:*" {
			continue
		}
		n++
		if tu.Condition == nil || tu.Condition.Name != "has_scope" {
			t.Fatalf("wildcard tuple on %s missing has_scope condition", tu.Object)
		}
		scopes, ok := tu.Condition.Context["granted_scopes"].(map[string]any)
		if !ok || len(scopes) == 0 {
			t.Fatalf("wildcard tuple on %s missing granted_scopes context: %v", tu.Object, tu.Condition.Context)
		}
	}
	if n == 0 {
		t.Fatal("no conditioned wildcard tuples generated; test is vacuous")
	}
}
