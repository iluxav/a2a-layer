// Package mcpproxy serves each task a filtered copy of its MCP servers.
//
// A CLI such as Claude Code loads the definition of every tool on every MCP server it is given,
// whatever it is allowed to call; a server with hundreds of tools fills the model's context
// before the task starts. So a task never talks to its servers directly. For each one, the
// proxy connects upstream (with the configured headers and the headers forwarded from the A2A
// request), keeps only the allowed tools, and serves that list at a private, per-task URL on
// the layer's own listener. The CLI sees exactly the agent's tool list, and nothing else can be
// called through it.
//
// The proxy's upstream client offers no interactive capabilities (no elicitation), so a server
// that asks a human before a risky call sends the ask to its own console or channel rather than
// to the headless CLI, which could not answer it.
package mcpproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PathPrefix is where the proxy's endpoints live on the listener: /mcp-proxy/<token>/<server>.
const PathPrefix = "/mcp-proxy/"

// Upstream is one MCP server a task may use.
type Upstream struct {
	Name    string
	URL     string
	Headers map[string]string
	// Tools is the allowlist; empty allows every tool the server offers.
	Tools []string
}

// Served is a proxied server as the task sees it.
type Served struct {
	Name string
	URL  string
	// Tools are the tools served (the allowlist, as far as the server offers them).
	Tools []string
	// Missing are allowlisted tools the server did not offer (renamed, removed, or the service
	// behind a gateway is down).
	Missing []string
}

// Proxy hosts the per-task endpoints.
type Proxy struct {
	base string // e.g. http://127.0.0.1:7300 — where the CLI reaches this listener

	mu       sync.Mutex
	sessions map[string]*session // "<token>/<server>" → session
}

type session struct {
	upstream *mcp.ClientSession
	server   *mcp.Server
	handler  http.Handler
}

// close ends the CLI's sessions on the filtered server (so no stream is left open) and the
// upstream session behind it.
func (s *session) close() {
	for ss := range s.server.Sessions() {
		ss.Close()
	}
	s.upstream.Close()
}

// New makes a proxy whose endpoints are reachable at base (no trailing slash).
func New(base string) *Proxy {
	return &Proxy{base: strings.TrimRight(base, "/"), sessions: map[string]*session{}}
}

// ServeHTTP routes /mcp-proxy/<token>/<server> to that task's filtered server.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
	p.mu.Lock()
	s := p.sessions[key]
	p.mu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	s.handler.ServeHTTP(w, r)
}

// Open connects to each upstream and serves its filtered copy for one task. The returned close
// function ends the task's endpoints and upstream sessions; call it when the task finishes.
func (p *Proxy) Open(ctx context.Context, upstreams []Upstream) ([]Served, func(), error) {
	token := randomToken()
	var (
		served []Served
		keys   []string
	)
	closeAll := func() {
		p.mu.Lock()
		var ss []*session
		for _, k := range keys {
			if s := p.sessions[k]; s != nil {
				ss = append(ss, s)
				delete(p.sessions, k)
			}
		}
		p.mu.Unlock()
		for _, s := range ss {
			s.close()
		}
	}
	for _, u := range upstreams {
		s, sv, err := connect(ctx, u)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("mcp server %s: %w", u.Name, err)
		}
		key := token + "/" + u.Name
		p.mu.Lock()
		p.sessions[key] = s
		p.mu.Unlock()
		keys = append(keys, key)
		sv.URL = p.base + PathPrefix + key
		served = append(served, sv)
	}
	return served, closeAll, nil
}

// connect opens the upstream session and builds the filtered server in front of it.
func connect(ctx context.Context, u Upstream) (*session, Served, error) {
	httpClient := &http.Client{Transport: headerTransport{headers: u.Headers, base: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "a2a-layer", Version: "1.0.0"}, nil)
	up, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: u.URL, HTTPClient: httpClient}, nil)
	if err != nil {
		return nil, Served{}, fmt.Errorf("connecting to %s: %w", u.URL, err)
	}
	allowed := map[string]bool{}
	for _, t := range u.Tools {
		allowed[t] = true
	}
	server := mcp.NewServer(&mcp.Implementation{Name: u.Name, Version: "1.0.0"}, nil)
	sv := Served{Name: u.Name}
	offered := map[string]bool{}
	for tool, err := range up.Tools(ctx, nil) {
		if err != nil {
			up.Close()
			return nil, Served{}, fmt.Errorf("listing tools: %w", err)
		}
		offered[tool.Name] = true
		if len(allowed) > 0 && !allowed[tool.Name] {
			continue
		}
		name := tool.Name
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return up.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
		})
		sv.Tools = append(sv.Tools, name)
	}
	for _, t := range u.Tools {
		if !offered[t] {
			sv.Missing = append(sv.Missing, t)
		}
	}
	slices.Sort(sv.Tools)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return &session{upstream: up, server: server, handler: handler}, sv, nil
}

// headerTransport adds fixed headers to every upstream request.
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

func randomToken() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
