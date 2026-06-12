package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(handler http.HandlerFunc) (*FGAClient, *httptest.Server) {
	srv := httptest.NewServer(handler)
	return NewFGAClient(OpenFGAConfig{APIURL: srv.URL, Timeout: 5 * time.Second}, 4), srv
}

func TestListStoresPaginates(t *testing.T) {
	calls := 0
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("continuation_token") {
		case "":
			json.NewEncoder(w).Encode(map[string]any{
				"stores":             []StoreInfo{{ID: "1", Name: "a"}, {ID: "2", Name: "b"}},
				"continuation_token": "next",
			})
		case "next":
			json.NewEncoder(w).Encode(map[string]any{
				"stores":             []StoreInfo{{ID: "3", Name: "c"}},
				"continuation_token": "",
			})
		default:
			t.Errorf("unexpected token %q", r.URL.Query().Get("continuation_token"))
		}
	})
	defer srv.Close()

	stores, err := client.ListStores()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(stores) != 3 || stores[2].ID != "3" {
		t.Errorf("got %d stores over %d calls: %+v", len(stores), calls, stores)
	}
}

func TestDeleteStore(t *testing.T) {
	var gotMethod, gotPath string
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	if err := client.DeleteStore("01ABC"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || gotPath != "/stores/01ABC" {
		t.Errorf("got %s %s, want DELETE /stores/01ABC", gotMethod, gotPath)
	}
}

func TestErrorResponsesAreSurfacedTruncated(t *testing.T) {
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'x'
	}
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(long)
	})
	defer srv.Close()

	err := client.DeleteStore("01ABC")
	if err == nil {
		t.Fatal("expected an error for HTTP 400")
	}
	if len(err.Error()) > 400 {
		t.Errorf("error not truncated: %d chars", len(err.Error()))
	}
}

func TestCheckRequestSerializesContextualTuples(t *testing.T) {
	var body map[string]any
	client, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	})
	defer srv.Close()

	_, err := client.Check("store1", CheckRequest{
		TupleKey: CheckTupleKey{User: "user:anne", Relation: "viewer", Object: "document:1"},
		ContextualTuples: &ContextualTupleKeys{TupleKeys: []TupleKey{{
			User:     "user:anne",
			Relation: "active_context",
			Object:   "document:1",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, ok := body["contextual_tuples"].(map[string]any)
	if !ok {
		t.Fatalf("contextual_tuples missing or wrong type: %#v", body["contextual_tuples"])
	}
	tuples, ok := ctx["tuple_keys"].([]any)
	if !ok || len(tuples) != 1 {
		t.Fatalf("tuple_keys missing or wrong size: %#v", ctx["tuple_keys"])
	}
}
