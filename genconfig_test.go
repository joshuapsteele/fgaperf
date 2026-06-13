package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated config must be a working starting point: load it through the
// real parser, validate against the model it was generated from, and confirm
// model-derived sections actually appeared.
func TestGenerateConfigRoundTrips(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := generateConfig("examples/model.json", a, &buf); err != nil {
		t.Fatal(err)
	}
	yaml := buf.String()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("generated config failed to parse: %v\n---\n%s", err, yaml)
	}
	if err := cfg.validateAgainstModel(a); err != nil {
		t.Fatalf("generated config failed model validation: %v", err)
	}

	if len(cfg.Probe.Targets) != len(a.AllRelations) {
		t.Errorf("expected %d probe targets (one per relation), got %d",
			len(a.AllRelations), len(cfg.Probe.Targets))
	}
	// The example model defines has_scope; expect a generated entry with the
	// map param on the tuple side and the scalar on the request side.
	cc, ok := cfg.Conditions["has_scope"]
	if !ok {
		t.Fatal("expected conditions block for has_scope")
	}
	if !contains(cc.TupleParams, "granted_scopes") {
		t.Errorf("granted_scopes (a map) should be tuple-bound, got tuple_params=%v", cc.TupleParams)
	}
	if _, ok := cc.ParamConfigs["required_scope"]; !ok {
		t.Errorf("required_scope param config missing: %+v", cc.ParamConfigs)
	}
	// And every condition param should reference an emitted pool.
	for cond, cc := range cfg.Conditions {
		for param, pc := range cc.ParamConfigs {
			if pc.Pool == "" {
				t.Errorf("%s.%s emitted without a pool", cond, param)
				continue
			}
			if _, ok := cfg.Pools[pc.Pool]; !ok {
				t.Errorf("%s.%s references pool %q, not in pools block", cond, param, pc.Pool)
			}
		}
	}
}

// User-like types (no relations of their own) should get a bigger instance
// pool than container types — that's the whole point of the heuristic.
func TestGenerateConfigSizesInstances(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	inst := pickInstances(a)
	if inst["user"] <= inst["document"] {
		t.Errorf("user-like type should outnumber container type: user=%d document=%d",
			inst["user"], inst["document"])
	}
}

// Userset acceptors should get a per-user-type fanout bump; self-recursive
// relations should pin to 1.
func TestGenerateConfigFanoutHeuristics(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	f := pickFanout(a)
	// folder#parent points back at folder (self-referential, no usersets).
	if got := f["folder#parent"]; got != 1 {
		t.Errorf("folder#parent should be pinned to 1, got %d", got)
	}
	// document#viewer accepts user, group#member, and user:* — the userset
	// arm should get bumped.
	key := "document#viewer@group#member"
	if got := f[key]; got <= 2 {
		t.Errorf("%s should be bumped above the default fanout, got %d", key, got)
	}
}

// Assignable relations not already tuned by the per-user-type bumps should
// appear as commented hints inside fanout:, so the operator can't miss them.
// Conditional details: the bare key for an auto-tuned relation (e.g. one
// pinned via self-reference) must NOT appear in the hint list, and
// contextual relations must be excluded.
func TestGenerateConfigListsRelationFanoutCandidates(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := generateConfig("examples/model.json", a, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// document#editor is assignable, isn't contextual, and isn't preset by
	// the auto fanouts — it must show up as a commented candidate.
	if !strings.Contains(out, "# document#editor:") {
		t.Errorf("expected commented fanout candidate for document#editor:\n%s", out)
	}
	// folder#parent was pinned to 1 (self-recursive) — its bare key is
	// already in the live fanout map, so it must NOT also appear commented.
	if strings.Contains(out, "# folder#parent:") {
		t.Errorf("folder#parent was pre-tuned; should not also appear as a commented candidate:\n%s", out)
	}
	// document#active_context is contextual — never seeded, never tuned by
	// fanout. Must not appear as a candidate.
	if strings.Contains(out, "# document#active_context:") {
		t.Errorf("contextual relation should not appear as a fanout candidate:\n%s", out)
	}
}

func TestGenerateConfigDoesNotGuessComputedContextualRelations(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	a.AllRelations = append(a.AllRelations, TypeRelation{Type: "document", Relation: "current_viewer"})
	a.TypeDefs["document"].Relations["current_viewer"] = Userset{
		ComputedUserset: &ObjectRelation{Relation: "viewer"},
	}

	guesses := guessContextualRelations(a)
	if contains(guesses, "document#current_viewer") {
		t.Fatalf("computed-only relation was guessed as contextual: %v", guesses)
	}
	if !contains(guesses, "document#active_context") {
		t.Fatalf("assignable contextual relation was not guessed: %v", guesses)
	}
}

// The model_file value in the emitted YAML should be the exact path passed
// in, not anything we tried to rewrite.
func TestGenerateConfigEchoesModelPath(t *testing.T) {
	a, err := LoadModel("examples/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := generateConfig("subdir/my-model.json", a, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "model_file: subdir/my-model.json") {
		t.Errorf("model_file path not echoed verbatim:\n%s", buf.String())
	}
}
