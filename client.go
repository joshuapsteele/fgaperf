package main

// client.go is a deliberately thin HTTP client for the OpenFGA REST API. We
// avoid the official SDK so the hot path is a single pre-tuned http.Client
// with no middleware, retries, or allocation surprises between us and the
// latency we are measuring.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type FGAClient struct {
	baseURL string
	token   string       // static pre-shared key; "" when using OIDC or no auth
	ts      *tokenSource // OIDC token source; nil otherwise
	http    *http.Client
}

func NewFGAClient(cfg OpenFGAConfig, maxConns int) *FGAClient {
	tr := &http.Transport{
		MaxIdleConns:        maxConns * 2,
		MaxIdleConnsPerHost: maxConns * 2,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
	}
	c := &FGAClient{
		baseURL: cfg.APIURL,
		token:   cfg.APIToken,
		http:    &http.Client{Transport: tr, Timeout: cfg.Timeout},
	}
	if cfg.OIDC != nil {
		// A separate client for token fetches: they happen off the request hot
		// path, so they don't need (or want) the tuned load transport.
		tokenHTTP := &http.Client{Timeout: cfg.Timeout}
		c.ts = newTokenSource(*cfg.OIDC, tokenHTTP)
	}
	return c
}

// tokenSource fetches and caches an OAuth2 client-credentials token, refreshing
// it in the background well before expiry so no request ever pays the fetch
// cost. Reads take a read lock; refreshes take the write lock.
type tokenSource struct {
	cfg  OIDCConfig
	http *http.Client
	mu   sync.RWMutex
	tok  string
	exp  time.Time
	err  error
}

func newTokenSource(cfg OIDCConfig, hc *http.Client) *tokenSource {
	ts := &tokenSource{cfg: cfg, http: hc}
	ts.refresh() // synchronous initial fetch so the first request has a token (or a clear error)
	go ts.loop()
	return ts
}

// token returns the cached bearer token, or the last fetch error if none has
// ever succeeded — so a request fails fast with the real cause instead of a 401.
func (ts *tokenSource) token() (string, error) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.tok == "" {
		if ts.err != nil {
			return "", ts.err
		}
		return "", fmt.Errorf("OIDC token not yet available")
	}
	return ts.tok, nil
}

func (ts *tokenSource) refresh() {
	tok, ttl, err := fetchToken(ts.http, ts.cfg)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if err != nil {
		ts.err = err
		return
	}
	ts.tok, ts.exp, ts.err = tok, time.Now().Add(ttl), nil
}

// loop refreshes the token ~30s before it expires (or retries soon after a
// failure), for the lifetime of the process.
func (ts *tokenSource) loop() {
	for {
		ts.mu.RLock()
		exp := ts.exp
		ts.mu.RUnlock()
		wait := time.Until(exp) - 30*time.Second
		if wait < time.Second {
			wait = time.Second // retry soon when exp is unset (failed) or very near
		}
		time.Sleep(wait)
		ts.refresh()
	}
}

// fetchToken performs the OAuth2 client-credentials grant and returns the token
// and its lifetime.
func fetchToken(hc *http.Client, cfg OIDCConfig) (string, time.Duration, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if cfg.Audience != "" {
		form.Set("audience", cfg.Audience)
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	req, err := http.NewRequest("POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("OIDC token request to %s: %w", cfg.TokenURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("OIDC token endpoint %s returned HTTP %d: %s", cfg.TokenURL, resp.StatusCode, truncate(string(data), 200))
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", 0, fmt.Errorf("decoding OIDC token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", 0, fmt.Errorf("OIDC token endpoint %s returned no access_token", cfg.TokenURL)
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute // no expires_in: assume a conservative lifetime
	}
	return body.AccessToken, ttl, nil
}

// HTTPError is a non-2xx response. Load-phase error classification needs the
// status code, not just a formatted string.
type HTTPError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c *FGAClient) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	bearer := c.token
	if c.ts != nil {
		bearer, err = c.ts.token()
		if err != nil {
			return fmt.Errorf("OIDC auth: %w", err)
		}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: truncate(string(data), 300)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (c *FGAClient) CreateStore(name string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	err := c.do("POST", "/stores", map[string]string{"name": name}, &resp)
	return resp.ID, err
}

func (c *FGAClient) DeleteStore(storeID string) error {
	return c.do("DELETE", "/stores/"+storeID, nil, nil)
}

type StoreInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *FGAClient) ListStores() ([]StoreInfo, error) {
	var out []StoreInfo
	token := ""
	for {
		var resp struct {
			Stores            []StoreInfo `json:"stores"`
			ContinuationToken string      `json:"continuation_token"`
		}
		q := url.Values{"page_size": {"100"}}
		if token != "" {
			q.Set("continuation_token", token)
		}
		path := "/stores?" + q.Encode()
		if err := c.do("GET", path, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Stores...)
		if resp.ContinuationToken == "" {
			return out, nil
		}
		token = resp.ContinuationToken
	}
}

func (c *FGAClient) WriteModel(storeID string, rawModel json.RawMessage) (string, error) {
	var resp struct {
		AuthorizationModelID string `json:"authorization_model_id"`
	}
	err := c.do("POST", "/stores/"+storeID+"/authorization-models", json.RawMessage(rawModel), &resp)
	return resp.AuthorizationModelID, err
}

type TupleKey struct {
	User      string          `json:"user"`
	Relation  string          `json:"relation"`
	Object    string          `json:"object"`
	Condition *TupleCondition `json:"condition,omitempty"`
}

type TupleCondition struct {
	Name    string         `json:"name"`
	Context map[string]any `json:"context,omitempty"`
}

func (c *FGAClient) WriteTuples(storeID, modelID string, tuples []TupleKey) error {
	body := map[string]any{
		"writes":                 map[string]any{"tuple_keys": tuples},
		"authorization_model_id": modelID,
	}
	return c.do("POST", "/stores/"+storeID+"/write", body, nil)
}

// deleteKey is a tuple key without a condition: the write endpoint rejects
// conditions on deletes.
type deleteKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

func (c *FGAClient) DeleteTuples(storeID, modelID string, tuples []TupleKey) error {
	keys := make([]deleteKey, len(tuples))
	for i, t := range tuples {
		keys[i] = deleteKey{User: t.User, Relation: t.Relation, Object: t.Object}
	}
	body := map[string]any{
		"deletes":                map[string]any{"tuple_keys": keys},
		"authorization_model_id": modelID,
	}
	return c.do("POST", "/stores/"+storeID+"/write", body, nil)
}

type CheckRequest struct {
	TupleKey             CheckTupleKey        `json:"tuple_key"`
	ContextualTuples     *ContextualTupleKeys `json:"contextual_tuples,omitempty"`
	Context              map[string]any       `json:"context,omitempty"`
	AuthorizationModelID string               `json:"authorization_model_id,omitempty"`
	Consistency          string               `json:"consistency,omitempty"`
}

type CheckTupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type ContextualTupleKeys struct {
	TupleKeys []TupleKey `json:"tuple_keys"`
}

func (c *FGAClient) Check(storeID string, req CheckRequest) (bool, error) {
	var resp struct {
		Allowed bool `json:"allowed"`
	}
	err := c.do("POST", "/stores/"+storeID+"/check", req, &resp)
	return resp.Allowed, err
}

type BatchCheckItem struct {
	TupleKey         CheckTupleKey        `json:"tuple_key"`
	ContextualTuples *ContextualTupleKeys `json:"contextual_tuples,omitempty"`
	Context          map[string]any       `json:"context,omitempty"`
	CorrelationID    string               `json:"correlation_id"`
}

type BatchCheckRequest struct {
	Checks               []BatchCheckItem `json:"checks"`
	AuthorizationModelID string           `json:"authorization_model_id,omitempty"`
	Consistency          string           `json:"consistency,omitempty"`
}

type BatchCheckResponse struct {
	Result map[string]struct {
		Allowed bool `json:"allowed"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"result"`
}

func (c *FGAClient) BatchCheck(storeID string, req BatchCheckRequest) (*BatchCheckResponse, error) {
	var resp BatchCheckResponse
	err := c.do("POST", "/stores/"+storeID+"/batch-check", req, &resp)
	return &resp, err
}

// ListObjectsRequest asks "which objects of Type does User have Relation to?".
// Contextual tuples use the same {tuple_keys:[...]} envelope as Check.
type ListObjectsRequest struct {
	Type                 string               `json:"type"`
	Relation             string               `json:"relation"`
	User                 string               `json:"user"`
	ContextualTuples     *ContextualTupleKeys `json:"contextual_tuples,omitempty"`
	Context              map[string]any       `json:"context,omitempty"`
	AuthorizationModelID string               `json:"authorization_model_id,omitempty"`
	Consistency          string               `json:"consistency,omitempty"`
}

type ListObjectsResponse struct {
	Objects []string `json:"objects"`
}

func (c *FGAClient) ListObjects(storeID string, req ListObjectsRequest) (*ListObjectsResponse, error) {
	var resp ListObjectsResponse
	err := c.do("POST", "/stores/"+storeID+"/list-objects", req, &resp)
	return &resp, err
}

// ListUsersRequest asks "which users matching UserFilters have Relation to
// Object?". Note the OpenFGA API quirk: ListUsers takes contextual_tuples as a
// bare array, unlike Check/ListObjects which wrap them in a tuple_keys object.
type ListUsersRequest struct {
	Object               ListUsersObject  `json:"object"`
	Relation             string           `json:"relation"`
	UserFilters          []UserTypeFilter `json:"user_filters"`
	ContextualTuples     []TupleKey       `json:"contextual_tuples,omitempty"`
	Context              map[string]any   `json:"context,omitempty"`
	AuthorizationModelID string           `json:"authorization_model_id,omitempty"`
	Consistency          string           `json:"consistency,omitempty"`
}

type ListUsersObject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type UserTypeFilter struct {
	Type     string `json:"type"`
	Relation string `json:"relation,omitempty"`
}

type ListUsersResponse struct {
	Users []ListUsersUser `json:"users"`
}

// ListUsersUser is one entry of a ListUsers result. Exactly one of the three
// shapes is set: a concrete user object, a userset, or a typed wildcard
// (user:*). We only need Object and Wildcard for verification.
type ListUsersUser struct {
	Object   *ListUsersObject `json:"object,omitempty"`
	Userset  json.RawMessage  `json:"userset,omitempty"`
	Wildcard *struct {
		Type string `json:"type"`
	} `json:"wildcard,omitempty"`
}

func (c *FGAClient) ListUsers(storeID string, req ListUsersRequest) (*ListUsersResponse, error) {
	var resp ListUsersResponse
	err := c.do("POST", "/stores/"+storeID+"/list-users", req, &resp)
	return &resp, err
}
