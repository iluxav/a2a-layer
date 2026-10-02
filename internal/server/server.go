// Package server serves every configured agent over A2A on one HTTP listener, each under its
// own path: /<agent> for JSON-RPC and /<agent>/.well-known/agent-card.json for its card. A task
// runs the agent's runner (a headless CLI) with the agent's instructions and MCP servers.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/mcpproxy"
	"github.com/iluxav/a2a-layer/internal/runner"
)

// maxRequestBody bounds one JSON-RPC request.
const maxRequestBody = 4 << 20

// Server is the A2A front for every agent in the config.
type Server struct {
	cfg    *config.Config
	agents map[string]*agent
	tasks  *store
	convs  *conversations  // what each remembering agent keeps per A2A context
	proxy  *mcpproxy.Proxy // each task's filtered MCP servers, on this listener
	log    *slog.Logger
	ctx    context.Context // canceled on shutdown, which cancels every running task
	stop   context.CancelFunc
}

// agent is one configured agent, ready to take tasks.
type agent struct {
	cfg    *config.Agent
	runner runner.Runner
	card   a2a.AgentCard
	slots  chan struct{} // MaxParallel tokens
	prompt string        // instructions + common instructions

	mcpServer *mcp.Server  // the agent as an MCP server, at /<agent>/mcp
	mcp       http.Handler // serves mcpServer
}

// Option adjusts a Server as New builds it.
type Option func(*Server)

// WithProxy has the server's tasks reach their MCP servers through p instead of a proxy of its
// own on cfg.Listen. The caller serves p (under mcpproxy.PathPrefix) at the base URL it was made
// with; servers that share a listener share its proxy, as the dashboard's playground does.
func WithProxy(p *mcpproxy.Proxy) Option {
	return func(s *Server) { s.proxy = p }
}

// New builds the server. runners maps each CLI's name (claude, codex) to the runner that runs
// it; see NewRunners.
func New(cfg *config.Config, runners map[string]runner.Runner, log *slog.Logger, opts ...Option) (*Server, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := Check(cfg, runners); err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, agents: map[string]*agent{}, tasks: newStore(), convs: newConversations(), log: log, ctx: ctx, stop: stop}
	for _, o := range opts {
		o(s)
	}
	if s.proxy == nil {
		s.proxy = mcpproxy.New(LoopbackURL(cfg.Listen))
	}
	for _, name := range cfg.AgentNames() {
		a := cfg.Agents[name]
		r := runners[a.CLI]
		prompt := strings.TrimSpace(a.Instructions)
		if c := strings.TrimSpace(cfg.CommonInstructions); c != "" {
			prompt = strings.TrimSpace(prompt + "\n\n" + c)
		}
		ag := &agent{cfg: a, runner: r, card: cardFor(cfg.PublicURL, a), slots: make(chan struct{}, a.MaxParallel), prompt: prompt}
		ag.mcpServer, ag.mcp = s.mcpEndpointFor(ag)
		s.agents[name] = ag
	}
	go s.sweepLoop()
	return s, nil
}

// Shutdown cancels every running task and ends every conversation (its sessions and
// directories are deleted: the conversations live in memory, so nothing could resume them).
func (s *Server) Shutdown() {
	s.stop()
	for _, a := range s.agents {
		for ss := range a.mcpServer.Sessions() {
			ss.Close() // so no MCP client's stream holds the listener open
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.convs.count() > 0 && time.Now().Before(deadline) {
		s.endConversations(s.convs.sweep(time.Now(), true))
		if s.convs.count() > 0 {
			time.Sleep(100 * time.Millisecond) // a canceled task is still leaving its conversation
		}
	}
}

// endConversations deletes what ended conversations kept: the CLI's sessions and the
// directory their tasks ran in.
func (s *Server) endConversations(ended []*conversation) {
	for _, c := range ended {
		if c.dir == "" {
			continue
		}
		if a := s.agents[c.agent]; a != nil {
			if f, ok := a.runner.(runner.SessionForgetter); ok {
				if err := f.ForgetSessions(c.dir); err != nil {
					s.log.Warn("could not delete a conversation's sessions", "agent", c.agent, "err", err)
				}
			}
		}
		os.RemoveAll(c.dir)
		s.log.Info("conversation ended", "agent", c.agent, "context", c.contextID)
	}
}

func (s *Server) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.tasks.sweep(now)
			s.endConversations(s.convs.sweep(now, false))
		}
	}
}

// cardFor builds an agent's card; its URL is the agent's own path on the public URL.
func cardFor(publicURL string, a *config.Agent) a2a.AgentCard {
	c := a2a.AgentCard{
		Name: a.Name, Description: a.Description, URL: publicURL + "/" + a.Name, Version: a.Version,
		ProtocolVersion: a2a.ProtocolVersion, PreferredTransport: "JSONRPC",
		DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
	}
	for _, sk := range a.Skills {
		tags := sk.Tags
		if tags == nil {
			tags = []string{}
		}
		c.Skills = append(c.Skills, a2a.Skill{ID: sk.ID, Name: sk.Name, Description: sk.Description, Tags: tags, Examples: sk.Examples})
	}
	if a.Secret != "" {
		c.SecuritySchemes = map[string]a2a.SecurityScheme{"bearer": {Type: "http", Scheme: "bearer"}}
		c.Security = []map[string][]string{{"bearer": {}}}
	}
	return c
}

// Check reports what the config asks that its CLIs cannot do: a CLI a2a-layer does not run,
// or built-in tools a CLI does not take (such as permission rules on one without them).
func Check(cfg *config.Config, runners map[string]runner.Runner) error {
	known := strings.Join(slices.Sorted(maps.Keys(runners)), ", ")
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(cfg.CLI)) {
		if runners[name] == nil {
			errs = append(errs, fmt.Errorf("cli %s: not a CLI a2a-layer runs (%s)", name, known))
		}
	}
	for _, name := range cfg.AgentNames() {
		a := cfg.Agents[name]
		r := runners[a.CLI]
		if r == nil {
			errs = append(errs, fmt.Errorf("agent %s: cli %q is not one a2a-layer runs (%s)", name, a.CLI, known))
			continue
		}
		if err := runner.CheckBuiltinTools(r, a.BuiltinTools); err != nil {
			errs = append(errs, fmt.Errorf("agent %s: cli %s: %w", name, a.CLI, err))
		}
	}
	return errors.Join(errs...)
}

// NewRunners builds a runner for every CLI a2a-layer knows, each as the config's cli section
// says to run it.
func NewRunners(cfg *config.Config) (map[string]runner.Runner, error) {
	runners := map[string]runner.Runner{}
	for _, name := range runner.Types() {
		c := cfg.CLI[name]
		r, err := runner.New(name, runner.Options{Command: c.Command, Args: c.Args, Env: c.Env})
		if err != nil {
			return nil, err
		}
		runners[name] = r
	}
	return runners, nil
}

// LoopbackURL is where a CLI on this machine reaches a listener: a wildcard bind address
// (0.0.0.0, ::, or none) is reached through 127.0.0.1.
func LoopbackURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Handler routes requests: the tasks' MCP proxy under /mcp-proxy/, and the agents.
func (s *Server) Handler() http.Handler {
	agents := s.agentHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, mcpproxy.PathPrefix) {
			s.proxy.ServeHTTP(w, r)
			return
		}
		agents.ServeHTTP(w, r)
	})
}

func (s *Server) agentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /{agent}/.well-known/agent-card.json", s.cardHandler)
	mux.HandleFunc("GET /{agent}/.well-known/agent.json", s.cardHandler)
	mux.HandleFunc("GET /{agent}", s.cardHandler)
	mux.HandleFunc("POST /{agent}", s.rpc)
	mux.HandleFunc("POST /{agent}/", s.rpc)
	// The agent as an MCP server. With methods, so these are more specific than POST /{agent}/.
	for _, method := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(method+" /{agent}/mcp", s.mcpEndpoint)
	}
	return mux
}

// index lists the agents and where their cards are.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
		Card        string `json:"card"`
	}
	out := []entry{}
	for _, name := range s.cfg.AgentNames() {
		a := s.agents[name]
		out = append(out, entry{Name: name, Description: a.card.Description, URL: a.card.URL, Card: a.card.URL + "/.well-known/agent-card.json"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *Server) cardHandler(w http.ResponseWriter, r *http.Request) {
	a := s.agents[r.PathValue("agent")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, a.card)
}

// rpc handles one JSON-RPC request to an agent.
func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	a := s.agents[r.PathValue("agent")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	if !authorized(w, r, a) {
		return
	}
	var req a2a.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &a2a.Error{Code: a2a.CodeParseError, Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, &a2a.Error{Code: a2a.CodeInvalidRequest, Message: "not a JSON-RPC 2.0 request"})
		return
	}
	var (
		result any
		rpcErr *a2a.Error
	)
	switch req.Method {
	case "message/send":
		result, rpcErr = s.send(r, a, req.Params)
	case "tasks/get":
		result, rpcErr = s.get(a, req.Params)
	case "tasks/cancel":
		result, rpcErr = s.cancel(a, req.Params)
	default:
		rpcErr = &a2a.Error{Code: a2a.CodeMethodNotFound, Message: "method " + req.Method + " is not supported (message/send, tasks/get, tasks/cancel are)"}
	}
	writeRPC(w, req.ID, result, rpcErr)
}

// mcpEndpoint serves the agent's MCP server, to callers with its secret.
func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	a := s.agents[r.PathValue("agent")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	if !authorized(w, r, a) {
		return
	}
	a.mcp.ServeHTTP(w, r)
}

// authorized checks the caller's bearer token against the agent's secret, answering 401 if
// it does not match.
func authorized(w http.ResponseWriter, r *http.Request, a *agent) bool {
	if a.cfg.Secret == "" || bearerMatches(r.Header.Get("Authorization"), a.cfg.Secret) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+a.cfg.Name+`"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	return false
}

// send starts a task. With configuration.blocking false it answers at once with the task,
// to be polled with tasks/get; otherwise it waits for the task to finish (or the caller to
// hang up) and answers with where it got to.
func (s *Server) send(r *http.Request, a *agent, raw json.RawMessage) (any, *a2a.Error) {
	var p a2a.SendParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &a2a.Error{Code: a2a.CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	prompt := strings.TrimSpace(p.Message.Text())
	if prompt == "" {
		return nil, &a2a.Error{Code: a2a.CodeUnsupportedContent, Message: "the message has no text"}
	}
	t := s.start(a, p.Message, prompt, r.Header)
	blocking := p.Configuration == nil || p.Configuration.Blocking == nil || *p.Configuration.Blocking
	if blocking {
		select {
		case <-t.done:
		case <-r.Context().Done():
		}
	}
	return t.snapshot(nil), nil
}

func (s *Server) get(a *agent, raw json.RawMessage) (any, *a2a.Error) {
	var p a2a.TaskParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" {
		return nil, &a2a.Error{Code: a2a.CodeInvalidParams, Message: "params need a task id"}
	}
	t := s.tasks.get(a.cfg.Name, p.ID)
	if t == nil {
		return nil, &a2a.Error{Code: a2a.CodeTaskNotFound, Message: "no such task"}
	}
	return t.snapshot(p.HistoryLength), nil
}

func (s *Server) cancel(a *agent, raw json.RawMessage) (any, *a2a.Error) {
	var p a2a.TaskParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ID == "" {
		return nil, &a2a.Error{Code: a2a.CodeInvalidParams, Message: "params need a task id"}
	}
	t := s.tasks.get(a.cfg.Name, p.ID)
	if t == nil {
		return nil, &a2a.Error{Code: a2a.CodeTaskNotFound, Message: "no such task"}
	}
	if !t.setState(a2a.StateCanceled, "canceled by the caller") {
		return nil, &a2a.Error{Code: a2a.CodeTaskNotCancelable, Message: "the task has already finished"}
	}
	t.cancel()
	s.log.Info("task canceled", "agent", a.cfg.Name, "task", t.id)
	return t.snapshot(nil), nil
}

// start creates a task and runs it in the background.
func (s *Server) start(a *agent, msg a2a.Message, prompt string, inbound http.Header) *task {
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(a.cfg.Timeout))
	contextID := msg.ContextID
	if contextID == "" {
		contextID = newID()
	}
	t := &task{
		id: newID(), contextID: contextID, agent: a.cfg.Name, request: msg,
		state: a2a.StateSubmitted, updated: time.Now(), cancel: cancel, done: make(chan struct{}),
		metadata: map[string]any{"cli": a.cfg.CLI, "model": a.cfg.Model},
	}
	t.request.TaskID, t.request.ContextID = t.id, contextID
	if t.request.Kind == "" {
		t.request.Kind = "message"
	}
	s.tasks.add(t)
	job := runner.Job{Prompt: prompt, Instructions: a.prompt, Model: a.cfg.Model, MaxTurns: a.cfg.MaxTurns, BuiltinTools: a.cfg.BuiltinTools}
	go s.run(ctx, a, t, job, upstreamsFor(a.cfg, inbound))
	return t
}

// upstreamsFor resolves an agent's MCP servers for one task, copying the forwarded headers
// from the request that started it.
func upstreamsFor(a *config.Agent, inbound http.Header) []mcpproxy.Upstream {
	var out []mcpproxy.Upstream
	for _, name := range slices.Sorted(maps.Keys(a.MCP)) {
		m := a.MCP[name]
		headers := map[string]string{}
		for k, v := range m.Headers {
			headers[k] = v
		}
		for _, h := range m.ForwardHeaders {
			if v := inbound.Get(h); v != "" {
				headers[h] = v
			}
		}
		out = append(out, mcpproxy.Upstream{Name: name, URL: m.URL, Headers: headers, Tools: m.Tools})
	}
	return out
}

// run waits for a free slot, opens the task's MCP proxy, runs the job, and records the outcome.
func (s *Server) run(ctx context.Context, a *agent, t *task, job runner.Job, upstreams []mcpproxy.Upstream) {
	defer t.cancel()
	// A remembering agent runs a context's tasks one at a time, in the context's own session.
	var conv *conversation
	if a.cfg.Context.Remember {
		c, err := s.convs.acquire(ctx, a.cfg.Name, t.contextID, time.Duration(a.cfg.Context.IdleTimeout))
		if err != nil {
			t.setState(a2a.StateFailed, "never started: "+reason(ctx))
			return
		}
		defer s.convs.release(c)
		conv = c
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-ctx.Done():
		t.setState(a2a.StateFailed, "never started: "+reason(ctx))
		return
	}
	if !t.setState(a2a.StateWorking, "started") {
		return // canceled while waiting for a slot
	}
	if conv != nil {
		if conv.dir == "" {
			dir, err := os.MkdirTemp(s.cfg.WorkDir, "a2a-"+a.cfg.Name+"-ctx-")
			if err != nil {
				t.setState(a2a.StateFailed, "could not create a work directory: "+err.Error())
				return
			}
			conv.dir = dir // kept for the conversation's next task; deleted when it ends
		}
		if conv.session != "" && conv.tasks >= a.cfg.Context.MaxTasks {
			// The session carries every earlier task; past the cap it starts afresh.
			s.log.Info("conversation session restarted", "agent", a.cfg.Name, "context", t.contextID, "after_tasks", conv.tasks)
			t.note("this conversation reached its limit of " + fmt.Sprint(a.cfg.Context.MaxTasks) + " tasks: starting a fresh session without the earlier ones")
			conv.session, conv.tasks = "", 0
		}
		job.WorkDir, job.Resume, job.KeepSession = conv.dir, conv.session, true
	} else {
		dir, err := os.MkdirTemp(s.cfg.WorkDir, "a2a-"+a.cfg.Name+"-")
		if err != nil {
			t.setState(a2a.StateFailed, "could not create a work directory: "+err.Error())
			return
		}
		defer os.RemoveAll(dir)
		job.WorkDir = dir
	}

	// The CLI gets the agent's MCP servers through the proxy: only the allowed tools, and the
	// configured and forwarded headers applied upstream, never shown to the CLI.
	served, closeProxy, err := s.proxy.Open(ctx, upstreams)
	if err != nil {
		t.setState(a2a.StateFailed, "could not reach its MCP servers: "+err.Error())
		s.log.Error("task failed", "agent", a.cfg.Name, "task", t.id, "err", err)
		return
	}
	defer closeProxy()
	var missing []string
	for _, sv := range served {
		job.MCP = append(job.MCP, runner.MCPServer{Name: sv.Name, URL: sv.URL, Tools: sv.Tools})
		for _, m := range sv.Missing {
			missing = append(missing, sv.Name+"/"+m)
		}
	}
	if len(missing) > 0 {
		// Usually a service behind a gateway that is down, or a tool renamed upstream: the agent
		// runs without them, and the task says so.
		s.log.Warn("tools not offered by their server", "agent", a.cfg.Name, "task", t.id, "missing", missing)
		t.mu.Lock()
		t.metadata["missing_tools"] = missing
		t.mu.Unlock()
		t.note("some of its tools are unavailable: " + strings.Join(missing, ", "))
	}

	started := time.Now()
	s.log.Info("task started", "agent", a.cfg.Name, "task", t.id, "model", job.Model)
	progress := func(note string) { t.note(truncate(note, 400)) }
	res, err := a.runner.Run(ctx, job, progress)
	if errors.Is(err, runner.ErrSessionNotFound) {
		// The session is gone (deleted outside the server): nothing ran, so start a new one.
		s.log.Warn("conversation session missing; starting a new one", "agent", a.cfg.Name, "context", t.contextID)
		job.Resume = ""
		res, err = a.runner.Run(ctx, job, progress)
	}
	elapsed := time.Since(started).Round(time.Second)
	if conv != nil && err == nil && res.SessionID != "" {
		conv.session = res.SessionID
		conv.tasks++
	}

	t.mu.Lock()
	t.metadata["duration_seconds"] = int(elapsed.Seconds())
	if err == nil {
		t.metadata["turns"] = res.Turns
		t.metadata["cost_usd"] = res.CostUSD
		t.metadata["input_tokens"] = res.InputTokens
		t.metadata["output_tokens"] = res.OutputTokens
		if conv != nil {
			t.metadata["context_task"] = conv.tasks // this task's place in the conversation's session
			t.metadata["resumed"] = job.Resume != ""
		}
		if res.Text != "" {
			t.artifacts = []a2a.Artifact{{ArtifactID: newID(), Parts: []a2a.Part{a2a.TextPart(res.Text)}}}
		}
	}
	t.mu.Unlock()

	switch {
	case err != nil && ctx.Err() != nil:
		if t.setState(a2a.StateFailed, "stopped: "+reason(ctx)) {
			s.log.Warn("task stopped", "agent", a.cfg.Name, "task", t.id, "after", elapsed, "why", reason(ctx))
		}
	case err != nil:
		t.setState(a2a.StateFailed, err.Error())
		s.log.Error("task failed", "agent", a.cfg.Name, "task", t.id, "after", elapsed, "err", err)
	case res.IsError:
		t.setState(a2a.StateFailed, res.Text)
		s.log.Warn("task did not finish", "agent", a.cfg.Name, "task", t.id, "after", elapsed, "turns", res.Turns, "why", res.Text)
	default:
		t.setState(a2a.StateCompleted, "")
		s.log.Info("task completed", "agent", a.cfg.Name, "task", t.id, "after", elapsed, "turns", res.Turns, "cost_usd", res.CostUSD)
	}
}

func reason(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "the task timed out"
	case ctx.Err() != nil:
		return "canceled"
	}
	return ""
}

func bearerMatches(header, secret string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, err *a2a.Error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	resp := a2a.Response{JSONRPC: "2.0", ID: id}
	if err != nil {
		resp.Error = err
	} else {
		resp.Result = result
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
