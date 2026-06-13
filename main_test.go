package main

import "testing"

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
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: -1}, 10); err == nil {
		t.Fatal("negative seeded_tuples passed validation")
	}
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: 11}, 10); err == nil {
		t.Fatal("out-of-range seeded_tuples passed validation")
	}
	if err := validateResumeState(State{TupleCount: 10, SeededTuples: 5}, 10); err != nil {
		t.Fatalf("valid resume state rejected: %v", err)
	}
}
