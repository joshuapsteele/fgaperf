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
