package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestContextualRelationsAreNotSeeded(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	cfg.Contextual.Relations = []string{"document#active_context"}
	w := NewWorld(a, cfg)

	for _, tu := range w.GenerateTuples() {
		if tu.Relation == "active_context" && strings.HasPrefix(tu.Object, "document:") {
			t.Fatalf("contextual relation was persisted: %+v", tu)
		}
	}
}

// The accept criterion for per-user-type fanout: a relation accepting
// [user, group#member] can get group members but no direct users.
func TestPerUserTypeFanoutOverride(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	cfg.Seed.Fanout = map[string]int{
		"document#editor@user":         0,
		"document#editor@group#member": 4,
	}
	w := NewWorld(a, cfg)

	users, usersets := 0, 0
	for _, tu := range w.GenerateTuples() {
		if tu.Relation != "editor" || !strings.HasPrefix(tu.Object, "document:") {
			continue
		}
		if strings.HasPrefix(tu.User, "user:") {
			users++
		}
		if strings.HasSuffix(tu.User, "#member") {
			usersets++
		}
	}
	if users != 0 {
		t.Errorf("document#editor got %d direct user tuples despite @user: 0", users)
	}
	if usersets == 0 {
		t.Error("document#editor got no group#member tuples despite @group#member: 4")
	}
}

func TestPerRelationWildcardProbability(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("") // global wildcard_probability defaults to 1.0
	cfg.Seed.WildcardProbs = map[string]float64{"document#viewer": 0}
	w := NewWorld(a, cfg)

	for _, tu := range w.GenerateTuples() {
		if strings.HasSuffix(tu.User, ":*") {
			t.Fatalf("wildcard tuple generated despite per-relation probability 0: %+v", tu)
		}
	}
}

// The accept criterion for keys_distribution: condition map sizes follow a
// configured bimodal distribution instead of one fixed count.
func TestKeysDistributionBimodal(t *testing.T) {
	a := loadExampleModel(t)
	cfg, _ := LoadConfigFile("")
	cfg.Conditions = map[string]CondConfig{
		"has_scope": {ParamConfigs: map[string]ParamGenConfig{
			"granted_scopes": {KeysDistribution: &KeysDistribution{
				Values: []int{1, 8}, Weights: []float64{0.5, 0.5},
			}},
		}},
	}
	w := NewWorld(a, cfg)

	sizes := map[int]int{}
	for _, tu := range w.GenerateTuples() {
		if tu.Condition == nil {
			continue
		}
		scopes, ok := tu.Condition.Context["granted_scopes"].(map[string]any)
		if !ok {
			continue
		}
		sizes[len(scopes)]++
	}
	for n := range sizes {
		if n != 1 && n != 8 {
			t.Errorf("map size %d generated; distribution only allows 1 or 8", n)
		}
	}
	if sizes[1] == 0 || sizes[8] == 0 {
		t.Fatalf("bimodal distribution did not produce both modes: %v", sizes)
	}
}

// Configs that don't use the new shaping knobs must consume the RNG exactly
// as before: bare-key fanout and the global wildcard probability take the
// same draws whether or not the per-type/per-relation code paths exist.
func TestDefaultConfigIgnoresShapingKnobs(t *testing.T) {
	a := loadExampleModel(t)
	plain, _ := LoadConfigFile("")
	knobbed, _ := LoadConfigFile("")
	knobbed.Seed.Fanout = map[string]int{"document#editor@user": 2} // 2 == default fanout
	knobbed.Seed.WildcardProbs = map[string]float64{"document#viewer": 1.0}

	first := NewWorld(a, plain).GenerateTuples()
	second := NewWorld(a, knobbed).GenerateTuples()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("no-op shaping knobs changed the generated tuple graph")
	}
}

// SeedStore must resume an interrupted seed without writing any tuple twice and
// without losing any: it skips the clean prefix it checkpointed, re-sends the
// post-hole batches, and tolerates the "already exists" rejection on the ones
// that had committed before the interruption. Modeled on OpenFGA's
// transactional, all-or-nothing batch writes.
func TestSeedStoreResume(t *testing.T) {
	const n = 25
	tuples := make([]TupleKey, n)
	for i := range tuples {
		tuples[i] = TupleKey{User: fmt.Sprintf("user:%d", i), Relation: "viewer", Object: "document:1"}
	}

	var mu sync.Mutex
	committed := map[string]int{} // user -> times written; >1 means a tuple was written twice
	failUser := "user:15"         // first run: fail the batch containing this user

	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Writes struct {
				TupleKeys []TupleKey `json:"tuple_keys"`
			} `json:"writes"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		batch := body.Writes.TupleKeys
		mu.Lock()
		defer mu.Unlock()
		// Transactional: if any tuple in the batch already exists, reject the
		// whole batch as a duplicate.
		for _, tk := range batch {
			if committed[tk.User] > 0 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":"validation_error","message":"cannot write a tuple which already exists"}`)
				return
			}
		}
		for _, tk := range batch {
			if tk.User == failUser {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		for _, tk := range batch {
			committed[tk.User]++
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	client := NewFGAClient(OpenFGAConfig{APIURL: srv.URL, Timeout: 5 * time.Second}, 4)

	cfg, _ := LoadConfigFile("")
	cfg.Seed.BatchSize = 10 // batches: [0-9], [10-19] (contains user:15), [20-24]
	cfg.Seed.Writers = 3

	var checkpoint int
	_, err := SeedStore(client, "s", "m", tuples, cfg, 0, false, func(w int) { checkpoint = w })
	if err == nil {
		t.Fatal("first run should fail (batch with user:15 errors)")
	}
	if checkpoint != 10 {
		t.Fatalf("checkpoint = %d, want 10 (clean prefix before the failed batch)", checkpoint)
	}

	// Resume: the failed batch now succeeds; the post-hole batch that committed
	// in run 1 must be tolerated, not double-written.
	failUser = ""
	var checkpoint2 int
	_, err = SeedStore(client, "s", "m", tuples, cfg, checkpoint, true, func(w int) { checkpoint2 = w })
	if err != nil {
		t.Fatalf("resume should succeed, got %v", err)
	}
	if checkpoint2 != n {
		t.Fatalf("resume checkpoint = %d, want %d", checkpoint2, n)
	}
	if len(committed) != n {
		t.Fatalf("committed %d distinct tuples, want %d", len(committed), n)
	}
	for u, c := range committed {
		if c != 1 {
			t.Errorf("tuple %s written %d times, want exactly 1", u, c)
		}
	}
}

// The batch watermark must only advance over a contiguous-from-zero run of
// completed batches, so the checkpoint it yields is always a clean tuple
// prefix that a resume can safely skip. A hole halts it.
func TestBatchWatermark(t *testing.T) {
	wm := &batchWatermark{seen: make([]bool, 4), sizes: []int{10, 10, 10, 5}}

	wm.complete(1) // batch 0 still missing -> no advance
	if wm.prefix != 0 {
		t.Fatalf("prefix advanced past a hole: %d", wm.prefix)
	}
	wm.complete(0) // now 0 and 1 are contiguous
	if wm.prefix != 20 {
		t.Fatalf("prefix = %d, want 20", wm.prefix)
	}
	wm.complete(2)
	if wm.prefix != 30 {
		t.Fatalf("prefix = %d, want 30", wm.prefix)
	}
	// batch 3 never completes (a failed write): the watermark must stay put.
	if wm.contig != 3 {
		t.Fatalf("contig = %d, want 3 (batch 3 incomplete)", wm.contig)
	}
}
