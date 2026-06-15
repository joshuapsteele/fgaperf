package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeFGAServer is a minimal in-process OpenFGA gRPC server: enough to exercise
// the GRPCClient end to end (dial, metadata, message conversion, response
// mapping) without Docker. UnimplementedOpenFGAServiceServer supplies the rest
// of the large service interface.
type fakeFGAServer struct {
	openfgav1.UnimplementedOpenFGAServiceServer
	mu        sync.Mutex
	lastCheck *openfgav1.CheckRequest
	writes    int
	deletes   int
}

func (s *fakeFGAServer) Check(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	s.mu.Lock()
	s.lastCheck = req
	s.mu.Unlock()
	return &openfgav1.CheckResponse{Allowed: true}, nil
}

func (s *fakeFGAServer) BatchCheck(ctx context.Context, req *openfgav1.BatchCheckRequest) (*openfgav1.BatchCheckResponse, error) {
	res := make(map[string]*openfgav1.BatchCheckSingleResult, len(req.GetChecks()))
	for _, it := range req.GetChecks() {
		res[it.GetCorrelationId()] = &openfgav1.BatchCheckSingleResult{
			CheckResult: &openfgav1.BatchCheckSingleResult_Allowed{Allowed: true},
		}
	}
	return &openfgav1.BatchCheckResponse{Result: res}, nil
}

func (s *fakeFGAServer) ListObjects(ctx context.Context, req *openfgav1.ListObjectsRequest) (*openfgav1.ListObjectsResponse, error) {
	return &openfgav1.ListObjectsResponse{Objects: []string{req.GetType() + ":1"}}, nil
}

func (s *fakeFGAServer) ListUsers(ctx context.Context, req *openfgav1.ListUsersRequest) (*openfgav1.ListUsersResponse, error) {
	return &openfgav1.ListUsersResponse{Users: []*openfgav1.User{
		{User: &openfgav1.User_Object{Object: &openfgav1.Object{Type: "user", Id: "1"}}},
		{User: &openfgav1.User_Wildcard{Wildcard: &openfgav1.TypedWildcard{Type: "user"}}},
	}}, nil
}

func (s *fakeFGAServer) Write(ctx context.Context, req *openfgav1.WriteRequest) (*openfgav1.WriteResponse, error) {
	s.mu.Lock()
	if w := req.GetWrites(); w != nil {
		s.writes += len(w.GetTupleKeys())
	}
	if d := req.GetDeletes(); d != nil {
		s.deletes += len(d.GetTupleKeys())
	}
	s.mu.Unlock()
	return &openfgav1.WriteResponse{}, nil
}

func startFakeGRPC(t *testing.T) (*fakeFGAServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &fakeFGAServer{}
	openfgav1.RegisterOpenFGAServiceServer(srv, fake)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return fake, lis.Addr().String()
}

func newTestGRPCClient(t *testing.T, addr string) *GRPCClient {
	t.Helper()
	gc, err := NewGRPCClient(OpenFGAConfig{GRPCURL: addr, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gc.Close() })
	return gc
}

// TestGRPCClientRunsAllEndpoints drives RunLoad over the gRPC transport for each
// of the four read endpoints and asserts the workers issued requests with no
// transport errors — the core acceptance criterion that gRPC runs all four.
func TestGRPCClientRunsAllEndpoints(t *testing.T) {
	_, addr := startFakeGRPC(t)
	gc := newTestGRPCClient(t, addr)

	corpus := &Corpus{StoreID: "store", ModelID: "model", Entries: []CorpusEntry{
		{User: "user:1", Relation: "viewer", Object: "document:1", Target: "document#viewer", Expected: true},
	}}
	for _, endpoint := range []string{"check", "batch-check", "list-objects", "list-users"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg, err := LoadConfigFile("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Load.Transport = "grpc"
			cfg.Load.Endpoint = singleEndpointMix(endpoint)
			cfg.Load.Concurrency = 2
			cfg.Load.Warmup = 20 * time.Millisecond
			cfg.Load.Duration = 80 * time.Millisecond
			res, err := RunLoad(gc, corpus, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.TotalChecks == 0 {
				t.Fatalf("%s: no requests issued", endpoint)
			}
			if res.TotalErrors != 0 {
				t.Fatalf("%s: %d transport errors, want 0 (classes: %v; samples: %v)", endpoint, res.TotalErrors, res.ErrorsByClass, res.ErrorSamples)
			}
		})
	}
}

func TestGRPCClientWriteDelete(t *testing.T) {
	fake, addr := startFakeGRPC(t)
	gc := newTestGRPCClient(t, addr)

	tuples := []TupleKey{{User: "user:churn-1", Relation: "viewer", Object: "document:churn-1"}}
	if err := gc.WriteTuples("store", "model", tuples); err != nil {
		t.Fatalf("WriteTuples: %v", err)
	}
	if err := gc.DeleteTuples("store", "model", tuples); err != nil {
		t.Fatalf("DeleteTuples: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writes != 1 || fake.deletes != 1 {
		t.Fatalf("server saw %d writes / %d deletes, want 1 / 1", fake.writes, fake.deletes)
	}
}

// TestGRPCCheckPropagatesContextAndContextualTuples verifies the structpb and
// tuple-condition conversion: the request-side context, a contextual tuple with
// a CEL condition (map/list/scalar values, exactly what seed.go generates), and
// the consistency preference all reach the server intact.
func TestGRPCCheckPropagatesContextAndContextualTuples(t *testing.T) {
	fake, addr := startFakeGRPC(t)
	gc := newTestGRPCClient(t, addr)

	allowed, err := gc.Check("store", CheckRequest{
		TupleKey: CheckTupleKey{User: "user:1", Relation: "viewer", Object: "document:1"},
		ContextualTuples: &ContextualTupleKeys{TupleKeys: []TupleKey{{
			User: "user:1", Relation: "member", Object: "group:1",
			Condition: &TupleCondition{Name: "has_scope", Context: map[string]any{
				"granted_scopes": []any{"read", "write"},
				"meta":           map[string]any{"k": "v"},
			}},
		}}},
		Context:              map[string]any{"required_scope": "read", "level": 3},
		AuthorizationModelID: "model",
		Consistency:          "HIGHER_CONSISTENCY",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !allowed {
		t.Fatal("expected allowed=true from fake server")
	}

	fake.mu.Lock()
	got := fake.lastCheck
	fake.mu.Unlock()
	if got == nil {
		t.Fatal("server received no Check request")
	}
	if got.GetConsistency() != openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY {
		t.Errorf("consistency = %v, want HIGHER_CONSISTENCY", got.GetConsistency())
	}
	if got.GetContext().GetFields()["required_scope"].GetStringValue() != "read" {
		t.Errorf("request context required_scope not propagated: %v", got.GetContext())
	}
	cts := got.GetContextualTuples().GetTupleKeys()
	if len(cts) != 1 {
		t.Fatalf("contextual tuples = %d, want 1", len(cts))
	}
	cond := cts[0].GetCondition()
	if cond.GetName() != "has_scope" {
		t.Errorf("condition name = %q, want has_scope", cond.GetName())
	}
	scopes := cond.GetContext().GetFields()["granted_scopes"].GetListValue().GetValues()
	if len(scopes) != 2 || scopes[0].GetStringValue() != "read" {
		t.Errorf("granted_scopes list not propagated: %v", scopes)
	}
}

func TestGRPCAddrDefault(t *testing.T) {
	cases := []struct {
		name string
		cfg  OpenFGAConfig
		want string
	}{
		{"explicit grpc_url wins", OpenFGAConfig{GRPCURL: "fga.example:9090", APIURL: "http://localhost:8080"}, "fga.example:9090"},
		{"derive host from api_url", OpenFGAConfig{APIURL: "http://openfga.internal:8080"}, "openfga.internal:8081"},
		{"derive bracketed ipv6 from api_url", OpenFGAConfig{APIURL: "http://[::1]:8080"}, "[::1]:8081"},
		{"localhost default", OpenFGAConfig{APIURL: "http://localhost:8080"}, "localhost:8081"},
		{"empty api_url falls back to localhost", OpenFGAConfig{}, "localhost:8081"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grpcAddr(tc.cfg); got != tc.want {
				t.Errorf("grpcAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGRPCErrClass(t *testing.T) {
	cases := []struct {
		code codes.Code
		want string
	}{
		{codes.DeadlineExceeded, "timeout"},
		{codes.Unavailable, "connection"},
		{codes.Canceled, "connection"},
		{codes.Internal, "5xx"},
		{codes.Unknown, "5xx"},
		{codes.InvalidArgument, "4xx"},
		{codes.NotFound, "4xx"},
		{codes.PermissionDenied, "4xx"},
	}
	for _, tc := range cases {
		err := status.Error(tc.code, "boom")
		if got := classifyErr(err); got != tc.want {
			t.Errorf("classifyErr(%v) = %q, want %q", tc.code, got, tc.want)
		}
	}
	// A non-gRPC error must fall through to the HTTP/net classification.
	if _, ok := grpcErrClass(context.Canceled); ok {
		t.Error("grpcErrClass claimed a plain (non-status) error")
	}
}
