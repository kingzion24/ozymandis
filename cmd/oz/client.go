package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to /api/v1.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient builds a client for a context.
//
// No overall timeout on the http.Client: `oz logs -f` holds a response open for
// as long as somebody watches it, and a client-level timeout would cut the
// stream at a fixed interval with no explanation. Requests that should be
// bounded carry their own context instead.
func NewClient(ctx Context) *Client {
	return &Client{
		endpoint: strings.TrimRight(ctx.Endpoint, "/"),
		token:    ctx.Token,
		http:     &http.Client{},
	}
}

// APIError is a failure the server described.
//
// Carries the code as well as the message so callers can branch — `oz deploy`
// distinguishes "no such app" from "you may not do that" — without matching on
// prose that is free to be reworded.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server returned %d", e.Status)
}

// NotFound reports whether this is a missing resource.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// do sends a request and decodes the response into out.
func (c *Client) do(
	ctx context.Context, method, path string, body, out any,
) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("oz: encode request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("oz: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("oz: cannot reach %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("oz: the server's reply was not the JSON this expects: %w", err)
	}
	return nil
}

// stream opens a response and hands the body to the caller to read.
//
// Separate from do because the caller has to be able to read as it arrives:
// decoding into a value would buffer a `logs -f` forever, which is the one
// thing that endpoint exists not to do.
func (c *Client) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("oz: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oz: cannot reach %s: %w", c.endpoint, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}

// decodeError turns a failed response into an APIError.
//
// A response that is not the JSON envelope is reported as what it was, not as a
// decode failure. The likely cause is pointing the CLI at something that is not
// an Ozymandis install — a proxy, a load balancer's error page — and "invalid
// character '<'" sends somebody looking in the wrong place.
func decodeError(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := json.Unmarshal(raw, &body); err != nil || body.Error.Code == "" {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{
			Status: resp.StatusCode,
			Message: fmt.Sprintf(
				"the server returned %d and something that is not an Ozymandis "+
					"API response. Is the endpoint right?\n%s", resp.StatusCode, msg),
		}
	}
	return &APIError{
		Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message,
	}
}

// --- Response shapes. Mirrors of the api package's, deliberately duplicated.
//
// The CLI does not import internal/api: that package pulls in the app service,
// which pulls in pgx and client-go, and a CLI binary carrying a Postgres driver
// to parse JSON is a binary nobody wants to ship. These are the wire contract,
// which is a smaller thing than the server's types and changes more slowly.

type Whoami struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
	Role     string `json:"role"`
}

type Status struct {
	Phase     string `json:"phase"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	Available int32  `json:"available"`
	Message   string `json:"message"`
}

type App struct {
	Name      string  `json:"name"`
	Image     string  `json:"image"`
	Replicas  int32   `json:"replicas"`
	Port      int32   `json:"port"`
	Source    string  `json:"source"`
	Internal  bool    `json:"internal"`
	Host      string  `json:"host"`
	TLS       bool    `json:"tls"`
	Status    *Status `json:"status"`
	CreatedAt string  `json:"created_at"`
}

// URL is where the app is reachable, or empty.
func (a App) URL() string {
	if a.Host == "" {
		return ""
	}
	if a.TLS {
		return "https://" + a.Host
	}
	return "http://" + a.Host
}

type Deployment struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Source   string `json:"source"`
	Image    string `json:"image"`
	Message  string `json:"message"`
	Finished bool   `json:"finished"`

	// ReleaseStatus answers "did my migrations run", which Status alone cannot.
	ReleaseStatus string `json:"release_status"`
	ReleaseLog    string `json:"release_log"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type Variable struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

type Change struct {
	Field   string `json:"field"`
	From    string `json:"from"`
	To      string `json:"to"`
	Axis    string `json:"axis"`
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason"`
}

type ConfigResult struct {
	Changes          []Change `json:"changes"`
	DryRun           bool     `json:"dry_run"`
	UntrackedDomains []string `json:"untracked_domains"`
}

type LogLine struct {
	At    time.Time `json:"at"`
	Text  string    `json:"text"`
	Error string    `json:"error"`
}

// --- Calls ---

func (c *Client) Whoami(ctx context.Context) (Whoami, error) {
	var out Whoami
	err := c.do(ctx, http.MethodGet, "/api/v1/whoami", nil, &out)
	return out, err
}

func (c *Client) Apps(ctx context.Context) ([]App, error) {
	var out struct {
		Apps []App `json:"apps"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/apps", nil, &out)
	return out.Apps, err
}

func (c *Client) App(ctx context.Context, name string) (App, error) {
	var out App
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+name, nil, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context, name string) (Status, error) {
	var out Status
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+name+"/status", nil, &out)
	return out, err
}

func (c *Client) Deployments(ctx context.Context, name string, limit int) ([]Deployment, error) {
	var out struct {
		Deployments []Deployment `json:"deployments"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/deployments?limit=%d", name, limit)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Deployments, err
}

func (c *Client) Deploy(ctx context.Context, name string) (Deployment, error) {
	var out struct {
		Deployment Deployment `json:"deployment"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+name+"/deploy", struct{}{}, &out)
	return out.Deployment, err
}

func (c *Client) Scale(ctx context.Context, name string, replicas int32) (App, error) {
	var out App
	body := map[string]any{"replicas": replicas}
	err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+name+"/scale", body, &out)
	return out, err
}

func (c *Client) Variables(ctx context.Context, name string) ([]Variable, error) {
	var out struct {
		Variables []Variable `json:"variables"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+name+"/secrets", nil, &out)
	return out.Variables, err
}

func (c *Client) SetVariables(
	ctx context.Context, name string, vars map[string]string, secret bool,
) error {
	body := map[string]any{"variables": vars, "secret": secret}
	return c.do(ctx, http.MethodPut, "/api/v1/apps/"+name+"/secrets", body, nil)
}

func (c *Client) DeleteVariable(ctx context.Context, name, key string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/apps/"+name+"/secrets/"+key, nil, nil)
}

func (c *Client) Config(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+name+"/config", nil, &out)
	return out, err
}

// PutConfig converges an app onto a spec, or previews it.
func (c *Client) PutConfig(
	ctx context.Context, name string, spec any, scale, dryRun bool,
) (ConfigResult, error) {
	path := "/api/v1/apps/" + name + "/config"
	if dryRun {
		path += "?dry_run=true"
	}
	body := map[string]any{"spec": spec, "scale": scale}

	var out ConfigResult
	err := c.do(ctx, http.MethodPut, path, body, &out)
	return out, err
}

func (c *Client) LogStream(ctx context.Context, name string, tail int) (io.ReadCloser, error) {
	return c.stream(ctx, fmt.Sprintf(
		"/api/v1/apps/%s/logs?follow=true&tail=%d", name, tail))
}

func (c *Client) Logs(ctx context.Context, name string, tail int) ([]LogLine, error) {
	var out struct {
		Lines []LogLine `json:"lines"`
		Note  string    `json:"note"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/logs?tail=%d", name, tail)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Lines, err
}
