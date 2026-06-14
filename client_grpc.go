package main

// client_grpc.go is the gRPC counterpart to client.go's HTTP client, selected
// by `load.transport: grpc`. OpenFGA's gRPC API is the lower-overhead
// production path; this lets a run answer "what does the server actually cost
// over gRPC" instead of baking HTTP+JSON serialization into the client-side
// numbers.
//
// Principle #3 (thin hot path) still holds: a single tuned grpc.ClientConn,
// no interceptors. Auth, when configured, rides as request metadata rather
// than an interceptor so the call path stays explicit. Only the load phase
// uses this; setup and probe stay on HTTP (client.go).

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type GRPCClient struct {
	conn    *grpc.ClientConn
	svc     openfgav1.OpenFGAServiceClient
	token   string       // static pre-shared key; "" when using OIDC or no auth
	ts      *tokenSource // OIDC token source; nil otherwise
	timeout time.Duration
}

var _ LoadClient = (*GRPCClient)(nil)

// grpcAddr resolves the gRPC dial target: the explicit openfga.grpc_url when
// set, otherwise the api_url's host with OpenFGA's default gRPC port (8081), so
// the bundled compose stack and the common localhost case work with no extra
// config.
func grpcAddr(cfg OpenFGAConfig) string {
	if cfg.GRPCURL != "" {
		return cfg.GRPCURL
	}
	host := "localhost"
	if u, err := url.Parse(cfg.APIURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return host + ":8081"
}

func NewGRPCClient(cfg OpenFGAConfig) (*GRPCClient, error) {
	var creds credentials.TransportCredentials
	if cfg.GRPCTLS {
		creds = credentials.NewTLS(&tls.Config{}) // system roots
	} else {
		creds = insecure.NewCredentials()
	}
	// One multiplexed connection, with generous HTTP/2 flow-control windows so
	// many concurrent in-flight checks don't stall on the default 64 KiB window.
	conn, err := grpc.NewClient(grpcAddr(cfg),
		grpc.WithTransportCredentials(creds),
		grpc.WithInitialWindowSize(1<<20),
		grpc.WithInitialConnWindowSize(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing gRPC %s: %w", grpcAddr(cfg), err)
	}
	c := &GRPCClient{
		conn:    conn,
		svc:     openfgav1.NewOpenFGAServiceClient(conn),
		token:   cfg.APIToken,
		timeout: cfg.Timeout,
	}
	if cfg.OIDC != nil {
		// Token fetches happen off the request hot path on their own HTTP client,
		// exactly as the HTTP transport does.
		c.ts = newTokenSource(*cfg.OIDC, &http.Client{Timeout: cfg.Timeout})
	}
	return c, nil
}

func (c *GRPCClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// callCtx builds the per-request context: a timeout (when configured) and the
// bearer token as gRPC metadata. OpenFGA accepts the same Authorization: Bearer
// header over gRPC that it does over HTTP, for both pre-shared keys and OIDC.
func (c *GRPCClient) callCtx() (context.Context, context.CancelFunc, error) {
	ctx := context.Background()
	cancel := func() {}
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	bearer := c.token
	if c.ts != nil {
		tok, err := c.ts.token()
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("OIDC auth: %w", err)
		}
		bearer = tok
	}
	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	}
	return ctx, cancel, nil
}

func grpcConsistency(s string) openfgav1.ConsistencyPreference {
	switch s {
	case "HIGHER_CONSISTENCY":
		return openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY
	case "MINIMIZE_LATENCY":
		return openfgav1.ConsistencyPreference_MINIMIZE_LATENCY
	default:
		return openfgav1.ConsistencyPreference_UNSPECIFIED
	}
}

// grpcStruct converts the request-side/condition context map to a protobuf
// Struct. The HTTP path sends this map as JSON; structpb uses the same
// JSON-value model (numbers become doubles, etc.), so semantics match.
func grpcStruct(m map[string]any) (*structpb.Struct, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(m)
}

func grpcTupleKey(t TupleKey) (*openfgav1.TupleKey, error) {
	tk := &openfgav1.TupleKey{User: t.User, Relation: t.Relation, Object: t.Object}
	if t.Condition != nil {
		ctx, err := grpcStruct(t.Condition.Context)
		if err != nil {
			return nil, err
		}
		tk.Condition = &openfgav1.RelationshipCondition{Name: t.Condition.Name, Context: ctx}
	}
	return tk, nil
}

func grpcContextual(c *ContextualTupleKeys) (*openfgav1.ContextualTupleKeys, error) {
	if c == nil || len(c.TupleKeys) == 0 {
		return nil, nil
	}
	out := make([]*openfgav1.TupleKey, len(c.TupleKeys))
	for i, t := range c.TupleKeys {
		tk, err := grpcTupleKey(t)
		if err != nil {
			return nil, err
		}
		out[i] = tk
	}
	return &openfgav1.ContextualTupleKeys{TupleKeys: out}, nil
}

func (c *GRPCClient) Check(storeID string, req CheckRequest) (bool, error) {
	ct, err := grpcContextual(req.ContextualTuples)
	if err != nil {
		return false, err
	}
	cx, err := grpcStruct(req.Context)
	if err != nil {
		return false, err
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return false, err
	}
	defer cancel()
	resp, err := c.svc.Check(ctx, &openfgav1.CheckRequest{
		StoreId:              storeID,
		TupleKey:             &openfgav1.CheckRequestTupleKey{User: req.TupleKey.User, Relation: req.TupleKey.Relation, Object: req.TupleKey.Object},
		ContextualTuples:     ct,
		Context:              cx,
		AuthorizationModelId: req.AuthorizationModelID,
		Consistency:          grpcConsistency(req.Consistency),
	})
	if err != nil {
		return false, err
	}
	return resp.GetAllowed(), nil
}

func (c *GRPCClient) BatchCheck(storeID string, req BatchCheckRequest) (*BatchCheckResponse, error) {
	items := make([]*openfgav1.BatchCheckItem, len(req.Checks))
	for i, it := range req.Checks {
		ct, err := grpcContextual(it.ContextualTuples)
		if err != nil {
			return nil, err
		}
		cx, err := grpcStruct(it.Context)
		if err != nil {
			return nil, err
		}
		items[i] = &openfgav1.BatchCheckItem{
			TupleKey:         &openfgav1.CheckRequestTupleKey{User: it.TupleKey.User, Relation: it.TupleKey.Relation, Object: it.TupleKey.Object},
			ContextualTuples: ct,
			Context:          cx,
			CorrelationId:    it.CorrelationID,
		}
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.svc.BatchCheck(ctx, &openfgav1.BatchCheckRequest{
		StoreId:              storeID,
		Checks:               items,
		AuthorizationModelId: req.AuthorizationModelID,
		Consistency:          grpcConsistency(req.Consistency),
	})
	if err != nil {
		return nil, err
	}
	// Map back onto the transport-agnostic shape load.go consumes.
	out := &BatchCheckResponse{Result: make(map[string]BatchCheckResult, len(resp.GetResult()))}
	for id, r := range resp.GetResult() {
		res := BatchCheckResult{Allowed: r.GetAllowed()}
		if e := r.GetError(); e != nil {
			res.Error = &BatchCheckError{Message: e.GetMessage()}
		}
		out.Result[id] = res
	}
	return out, nil
}

func (c *GRPCClient) ListObjects(storeID string, req ListObjectsRequest) (*ListObjectsResponse, error) {
	ct, err := grpcContextual(req.ContextualTuples)
	if err != nil {
		return nil, err
	}
	cx, err := grpcStruct(req.Context)
	if err != nil {
		return nil, err
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.svc.ListObjects(ctx, &openfgav1.ListObjectsRequest{
		StoreId:              storeID,
		AuthorizationModelId: req.AuthorizationModelID,
		Type:                 req.Type,
		Relation:             req.Relation,
		User:                 req.User,
		ContextualTuples:     ct,
		Context:              cx,
		Consistency:          grpcConsistency(req.Consistency),
	})
	if err != nil {
		return nil, err
	}
	return &ListObjectsResponse{Objects: resp.GetObjects()}, nil
}

func (c *GRPCClient) ListUsers(storeID string, req ListUsersRequest) (*ListUsersResponse, error) {
	cts := make([]*openfgav1.TupleKey, len(req.ContextualTuples))
	for i, t := range req.ContextualTuples {
		tk, err := grpcTupleKey(t)
		if err != nil {
			return nil, err
		}
		cts[i] = tk
	}
	cx, err := grpcStruct(req.Context)
	if err != nil {
		return nil, err
	}
	filters := make([]*openfgav1.UserTypeFilter, len(req.UserFilters))
	for i, f := range req.UserFilters {
		filters[i] = &openfgav1.UserTypeFilter{Type: f.Type, Relation: f.Relation}
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.svc.ListUsers(ctx, &openfgav1.ListUsersRequest{
		StoreId:              storeID,
		AuthorizationModelId: req.AuthorizationModelID,
		Object:               &openfgav1.Object{Type: req.Object.Type, Id: req.Object.ID},
		Relation:             req.Relation,
		UserFilters:          filters,
		ContextualTuples:     cts,
		Context:              cx,
		Consistency:          grpcConsistency(req.Consistency),
	})
	if err != nil {
		return nil, err
	}
	// Verification only inspects concrete users and typed wildcards, so map just
	// those two shapes (usersets are left nil, matching the HTTP decode that
	// never populates them for these checks).
	out := &ListUsersResponse{Users: make([]ListUsersUser, len(resp.GetUsers()))}
	for i, u := range resp.GetUsers() {
		var uu ListUsersUser
		if o := u.GetObject(); o != nil {
			uu.Object = &ListUsersObject{Type: o.GetType(), ID: o.GetId()}
		}
		if w := u.GetWildcard(); w != nil {
			uu.Wildcard = &ListUsersWildcard{Type: w.GetType()}
		}
		out.Users[i] = uu
	}
	return out, nil
}

func (c *GRPCClient) WriteTuples(storeID, modelID string, tuples []TupleKey) error {
	keys := make([]*openfgav1.TupleKey, len(tuples))
	for i, t := range tuples {
		tk, err := grpcTupleKey(t)
		if err != nil {
			return err
		}
		keys[i] = tk
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.svc.Write(ctx, &openfgav1.WriteRequest{
		StoreId:              storeID,
		Writes:               &openfgav1.WriteRequestWrites{TupleKeys: keys},
		AuthorizationModelId: modelID,
	})
	return err
}

func (c *GRPCClient) DeleteTuples(storeID, modelID string, tuples []TupleKey) error {
	keys := make([]*openfgav1.TupleKeyWithoutCondition, len(tuples))
	for i, t := range tuples {
		keys[i] = &openfgav1.TupleKeyWithoutCondition{User: t.User, Relation: t.Relation, Object: t.Object}
	}
	ctx, cancel, err := c.callCtx()
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.svc.Write(ctx, &openfgav1.WriteRequest{
		StoreId:              storeID,
		Deletes:              &openfgav1.WriteRequestDeletes{TupleKeys: keys},
		AuthorizationModelId: modelID,
	})
	return err
}

// grpcErrClass maps a gRPC status code onto load.go's coarse error classes so
// the report's error breakdown reads the same regardless of transport. ok is
// false for non-gRPC errors, leaving classifyErr's HTTP/net branches in charge.
func grpcErrClass(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() == codes.OK {
		return "", false
	}
	switch st.Code() {
	case codes.DeadlineExceeded:
		return "timeout", true
	case codes.Unavailable, codes.Canceled:
		return "connection", true
	case codes.Internal, codes.Unknown, codes.DataLoss:
		return "5xx", true
	default:
		// InvalidArgument, NotFound, PermissionDenied, ResourceExhausted, … —
		// the gRPC analogues of 4xx client errors.
		return "4xx", true
	}
}
