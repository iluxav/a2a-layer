package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/runner"
)

// fakeRunner stands in for a CLI. Like one, it connects to the job's MCP servers (the task's
// proxy), lists their tools and calls the first; then it answers "echo: <prompt>", after
// release is closed (if set).
type fakeRunner struct {
	mu      sync.Mutex
	jobs    []runner.Job
	seen    [][]string // per job: the tools the MCP servers listed
	called  []string   // per job: what the first tool call returned
	release chan struct{}
	started chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, job runner.Job, progress runner.Progress) (runner.Result, error) {
	var tools []string
	called := ""
	for _, m := range job.MCP {
		c := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "1"}, nil)
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: m.URL}, nil)
		if err != nil {
			return runner.Result{}, err
		}
		for tool, err := range sess.Tools(ctx, nil) {
			if err != nil {
				return runner.Result{}, err
			}
			tools = append(tools, tool.Name)
		}
		if len(tools) > 0 {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tools[0]})
			if err != nil {
				return runner.Result{}, err
			}
			called = res.Content[0].(*mcp.TextContent).Text
		}
		sess.Close()
	}
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.seen = append(f.seen, tools)
	f.called = append(f.called, called)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	progress("calling linear__get_issue")
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
	return runner.Result{Text: "echo: " + job.Prompt, Turns: 2, CostUSD: 0.01}, nil
}

// fakeGateway is an upstream MCP server with three tools, each answering with the headers its
// call arrived with.
func fakeGateway(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "1"}, nil)
	for _, name := range []string{"linear__get_issue", "linear__save_issue", "vercel__buy_domain"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				h := req.Extra.Header
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " auth=" + h.Get("Authorization") + " session=" + h.Get("X-Parent-Session")}}}, nil
			})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func (f *fakeRunner) lastJob() runner.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[len(f.jobs)-1]
}

func newTestServer(t *testing.T, f *fakeRunner, maxParallel int) *httptest.Server {
	return newTestServerWith(t, f, maxParallel, fakeGateway(t))
}

func newTestServerWith(t *testing.T, f *fakeRunner, maxParallel int, gatewayURL string) *httptest.Server {
	t.Helper()
	// Listen first, so the server knows the address its tasks' MCP proxy is reached at.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen:             ln.Addr().String(),
		PublicURL:          "http://agents.local:7300",
		WorkDir:            t.TempDir(),
		CommonInstructions: "Be brief.",
		Runners:            map[string]config.Runner{"claude": {Type: "claude"}},
		Agents: map[string]*config.Agent{
			"pm": {
				Name: "pm", Description: "Plans.", Version: "1.0.0", Instructions: "You are the PM.",
				Runner: "claude", Model: "haiku", MaxTurns: 9, Timeout: config.Duration(time.Minute),
				MaxParallel: maxParallel, Secret: "s3cret",
				Skills: []config.Skill{{ID: "plan_project", Name: "Plan", Description: "Plans a project."}},
				MCP: map[string]config.MCPServer{"gw": {
					URL: gatewayURL, Headers: map[string]string{"Authorization": "Bearer dgk_pm"},
					ForwardHeaders: []string{"X-Parent-Session"}, Tools: []string{"linear__get_issue", "linear__gone"},
				}},
			},
			"open": {Name: "open", Description: "No secret.", Version: "1.0.0", Runner: "claude", MaxTurns: 3, Timeout: config.Duration(time.Minute), MaxParallel: 1,
				Skills: []config.Skill{{ID: "open", Name: "open", Description: "Open."}}},
		},
	}
	s, err := New(cfg, map[string]runner.Runner{"claude": f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, url, token, method string, params any, header http.Header) a2a.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out a2a.Response
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func asTask(t *testing.T, r a2a.Response) a2a.Task {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("rpc error: %+v", r.Error)
	}
	b, _ := json.Marshal(r.Result)
	var task a2a.Task
	json.Unmarshal(b, &task)
	return task
}

func sendParams(text string, blocking *bool) map[string]any {
	p := map[string]any{"message": map[string]any{"kind": "message", "role": "user", "messageId": "m1", "parts": []map[string]any{{"kind": "text", "text": text}}}}
	if blocking != nil {
		p["configuration"] = map[string]any{"blocking": *blocking}
	}
	return p
}

func waitFor(t *testing.T, ts *httptest.Server, id, state string) a2a.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "tasks/get", map[string]any{"id": id}, nil))
		if task.Status.State == state || time.Now().After(deadline) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCardsLiveUnderEachAgentsPath(t *testing.T) {
	ts := newTestServer(t, &fakeRunner{}, 1)
	for _, path := range []string{"/pm/.well-known/agent-card.json", "/pm/.well-known/agent.json", "/pm"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", path, err, resp)
		}
		var card map[string]any
		json.NewDecoder(resp.Body).Decode(&card)
		resp.Body.Close()
		if card["url"] != "http://agents.local:7300/pm" || card["protocolVersion"] != a2a.ProtocolVersion {
			t.Errorf("%s: url %v version %v", path, card["url"], card["protocolVersion"])
		}
		if card["securitySchemes"] == nil {
			t.Errorf("%s: an agent with a secret advertises bearer auth", path)
		}
	}
	resp, _ := http.Get(ts.URL + "/nobody/.well-known/agent-card.json")
	if resp.StatusCode != 404 {
		t.Errorf("unknown agent: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/")
	var idx struct {
		Agents []struct{ Name, URL string } `json:"agents"`
	}
	json.NewDecoder(resp.Body).Decode(&idx)
	if len(idx.Agents) != 2 || idx.Agents[0].Name != "open" || idx.Agents[1].URL != "http://agents.local:7300/pm" {
		t.Errorf("index = %+v", idx)
	}
}

func TestSecretIsRequired(t *testing.T) {
	ts := newTestServer(t, &fakeRunner{}, 1)
	for _, token := range []string{"", "wrong"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"x"}}`
		req, _ := http.NewRequest("POST", ts.URL+"/pm", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", token, resp.StatusCode)
		}
	}
	// an agent without a secret takes anyone
	r := call(t, ts.URL+"/open", "", "tasks/get", map[string]any{"id": "x"}, nil)
	if r.Error == nil || r.Error.Code != a2a.CodeTaskNotFound {
		t.Errorf("open agent: %+v", r.Error)
	}
}

func TestNonBlockingSendIsPolledToCompletion(t *testing.T) {
	f := &fakeRunner{release: make(chan struct{})}
	ts := newTestServer(t, f, 1)
	no := false
	h := http.Header{"X-Parent-Session": {"sess_parent"}, "X-Other": {"not forwarded"}}
	task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("plan Brewline", &no), h))
	if a2a.Terminal(task.Status.State) || task.ID == "" || task.ContextID == "" {
		t.Fatalf("a non-blocking send answers at once with a live task: %+v", task)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		working := waitFor(t, ts, task.ID, a2a.StateWorking)
		if working.Status.Message != nil && strings.Contains(working.Status.Message.Text(), "calling linear__get_issue") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress should show in the status: %+v", working.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(f.release)
	done := waitFor(t, ts, task.ID, a2a.StateCompleted)
	if len(done.Artifacts) != 1 || done.Artifacts[0].Parts[0].Text != "echo: plan Brewline" || done.Artifacts[0].Name != "" {
		t.Errorf("artifacts = %+v", done.Artifacts)
	}
	if done.Metadata["cost_usd"] != 0.01 {
		t.Errorf("metadata = %v", done.Metadata)
	}

	job := f.lastJob()
	if job.Model != "haiku" || job.MaxTurns != 9 || job.Instructions != "You are the PM.\n\nBe brief." {
		t.Errorf("job = %+v", job)
	}
	// The CLI is pointed at the task's proxy, never at the server itself, and holds no secrets.
	if len(job.MCP) != 1 || !strings.HasPrefix(job.MCP[0].URL, ts.URL+"/mcp-proxy/") || len(job.MCP[0].Headers) != 0 {
		t.Fatalf("mcp = %+v", job.MCP)
	}
	f.mu.Lock()
	seen, called := f.seen[len(f.seen)-1], f.called[len(f.called)-1]
	f.mu.Unlock()
	// Through the proxy the CLI sees only its allowed tools, and the server receives the
	// agent's key and the session of the request that started the task.
	if !slices.Equal(seen, []string{"linear__get_issue"}) {
		t.Errorf("the CLI saw tools %v, want only linear__get_issue", seen)
	}
	if called != "linear__get_issue auth=Bearer dgk_pm session=sess_parent" {
		t.Errorf("the upstream saw %q", called)
	}
	if got := done.Metadata["missing_tools"]; got == nil || !strings.Contains(strings.Join(anyStrings(got), ","), "gw/linear__gone") {
		t.Errorf("an allowed tool the server does not offer should be reported, metadata = %v", done.Metadata)
	}
	// The proxy endpoint is gone once the task has finished.
	resp, err := http.Post(job.MCP[0].URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the task's proxy is still serving after it finished: %d", resp.StatusCode)
	}
}

func anyStrings(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestAnUnreachableMCPServerFailsTheTaskClearly(t *testing.T) {
	ts := newTestServerWith(t, &fakeRunner{}, 1, "http://127.0.0.1:1/mcp")
	task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("plan it", nil), nil))
	if task.Status.State != a2a.StateFailed || task.Status.Message == nil || !strings.Contains(task.Status.Message.Text(), "could not reach its MCP servers") {
		t.Errorf("task = %+v", task.Status)
	}
}

func TestBlockingSendWaitsForTheResult(t *testing.T) {
	ts := newTestServer(t, &fakeRunner{}, 1)
	task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("hello", nil), nil))
	if task.Status.State != a2a.StateCompleted || task.Artifacts[0].Parts[0].Text != "echo: hello" {
		t.Errorf("task = %+v", task)
	}
}

func TestTasksBeyondMaxParallelWaitTheirTurn(t *testing.T) {
	f := &fakeRunner{release: make(chan struct{}), started: make(chan struct{}, 4)}
	ts := newTestServer(t, f, 1)
	no := false
	first := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("one", &no), nil))
	<-f.started
	second := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("two", &no), nil))
	time.Sleep(50 * time.Millisecond)
	if got := waitFor(t, ts, second.ID, a2a.StateSubmitted).Status.State; got != a2a.StateSubmitted {
		t.Errorf("the second task should wait for the slot, got %s", got)
	}
	close(f.release)
	waitFor(t, ts, first.ID, a2a.StateCompleted)
	if got := waitFor(t, ts, second.ID, a2a.StateCompleted).Status.State; got != a2a.StateCompleted {
		t.Errorf("second = %s", got)
	}
}

func TestCancelStopsARunningTask(t *testing.T) {
	f := &fakeRunner{release: make(chan struct{}), started: make(chan struct{}, 1)}
	ts := newTestServer(t, f, 1)
	no := false
	task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("long", &no), nil))
	<-f.started
	canceled := asTask(t, call(t, ts.URL+"/pm", "s3cret", "tasks/cancel", map[string]any{"id": task.ID}, nil))
	if canceled.Status.State != a2a.StateCanceled {
		t.Errorf("state = %s", canceled.Status.State)
	}
	r := call(t, ts.URL+"/pm", "s3cret", "tasks/cancel", map[string]any{"id": task.ID}, nil)
	if r.Error == nil || r.Error.Code != a2a.CodeTaskNotCancelable {
		t.Errorf("second cancel: %+v", r.Error)
	}
	if got := waitFor(t, ts, task.ID, a2a.StateCanceled).Status.State; got != a2a.StateCanceled {
		t.Errorf("a canceled task stays canceled, got %s", got)
	}
}

func TestRPCErrors(t *testing.T) {
	ts := newTestServer(t, &fakeRunner{}, 1)
	if r := call(t, ts.URL+"/pm", "s3cret", "message/stream", map[string]any{}, nil); r.Error == nil || r.Error.Code != a2a.CodeMethodNotFound {
		t.Errorf("unknown method: %+v", r.Error)
	}
	if r := call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("  ", nil), nil); r.Error == nil || r.Error.Code != a2a.CodeUnsupportedContent {
		t.Errorf("empty message: %+v", r.Error)
	}
	if r := call(t, ts.URL+"/pm", "s3cret", "tasks/get", map[string]any{"id": "nope"}, nil); r.Error == nil || r.Error.Code != a2a.CodeTaskNotFound {
		t.Errorf("unknown task: %+v", r.Error)
	}
	// a task belongs to its agent: another agent cannot read it
	task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "message/send", sendParams("mine", nil), nil))
	if r := call(t, ts.URL+"/open", "", "tasks/get", map[string]any{"id": task.ID}, nil); r.Error == nil || r.Error.Code != a2a.CodeTaskNotFound {
		t.Errorf("cross-agent read: %+v", r.Error)
	}
}
