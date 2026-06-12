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
	"time"
)

type FGAClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewFGAClient(cfg OpenFGAConfig, maxConns int) *FGAClient {
	tr := &http.Transport{
		MaxIdleConns:        maxConns * 2,
		MaxIdleConnsPerHost: maxConns * 2,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
	}
	return &FGAClient{
		baseURL: cfg.APIURL,
		token:   cfg.APIToken,
		http:    &http.Client{Transport: tr, Timeout: cfg.Timeout},
	}
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
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
		path := "/stores?page_size=100"
		if token != "" {
			path += "&continuation_token=" + token
		}
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

type CheckRequest struct {
	TupleKey             CheckTupleKey  `json:"tuple_key"`
	Context              map[string]any `json:"context,omitempty"`
	AuthorizationModelID string         `json:"authorization_model_id,omitempty"`
	Consistency          string         `json:"consistency,omitempty"`
}

type CheckTupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

func (c *FGAClient) Check(storeID string, req CheckRequest) (bool, error) {
	var resp struct {
		Allowed bool `json:"allowed"`
	}
	err := c.do("POST", "/stores/"+storeID+"/check", req, &resp)
	return resp.Allowed, err
}

type BatchCheckItem struct {
	TupleKey      CheckTupleKey  `json:"tuple_key"`
	Context       map[string]any `json:"context,omitempty"`
	CorrelationID string         `json:"correlation_id"`
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
