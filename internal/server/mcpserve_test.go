package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/iluxav/a2a-layer/internal/config"
)

// headers adds fixed headers to every request, as an MCP client configured with them does.
type headers map[string]string

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// mcpClient connects to an agent's MCP endpoint, sending h on every request.
func mcpClient(t *testing.T, url string, h headers, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, opts)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: h}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func callTool(t *testing.T, sess *mcp.ClientSession, params *mcp.CallToolParams) (*mcp.CallToolResult, taskOutput) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	var out taskOutput
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return res, out
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestAnAgentIsAlsoAnMCPServer(t *testing.T) {
	f := &fakeRunner{}
	ts := newTestServer(t, f, 1)
	sess := mcpClient(t, ts.URL+"/pm/mcp", headers{"Authorization": "Bearer s3cret", "X-Parent-Session": "sess_mcp"}, nil)

	var names []string
	for tool, err := range sess.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
		if tool.Name == "plan_project" && (!strings.Contains(tool.Description, "Plans a project.") || !strings.Contains(tool.Description, "get_task")) {
			t.Errorf("plan_project's description: %q", tool.Description)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "cancel_task,get_task,plan_project" {
		t.Fatalf("tools = %v", names)
	}

	res, out := callTool(t, sess, &mcp.CallToolParams{Name: "plan_project", Arguments: map[string]any{"message": "plan the site"}})
	if res.IsError || text(res) != "echo: plan the site" || out.State != "completed" || out.Answer != "echo: plan the site" || out.TaskID == "" || out.ContextID == "" {
		t.Errorf("result %q, %+v", text(res), out)
	}
	// The task ran as an A2A one does: the agent's instructions, its MCP servers, and the
	// forwarded header from the MCP request.
	job := f.lastJob()
	if job.Prompt != "plan the site" || !strings.Contains(job.Instructions, "You are the PM.") {
		t.Errorf("job = %+v", job)
	}
	f.mu.Lock()
	called := f.called[len(f.called)-1]
	f.mu.Unlock()
	if called != "linear__get_issue auth=Bearer dgk_pm session=sess_mcp" {
		t.Errorf("upstream call = %q", called)
	}
	// It is the same task A2A sees.
	if task := asTask(t, call(t, ts.URL+"/pm", "s3cret", "tasks/get", map[string]any{"id": out.TaskID}, nil)); task.Status.State != "completed" {
		t.Errorf("over A2A the task is %s", task.Status.State)
	}

	if res, _ := callTool(t, sess, &mcp.CallToolParams{Name: "plan_project", Arguments: map[string]any{"message": "  "}}); !res.IsError || !strings.Contains(text(res), "message is empty") {
		t.Errorf("an empty message: %q", text(res))
	}
}

func TestTheMCPEndpointNeedsTheSecret(t *testing.T) {
	ts := newTestServer(t, &fakeRunner{}, 1)
	for _, token := range []string{"", "wrong"} {
		req, _ := http.NewRequest("POST", ts.URL+"/pm/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("token %q: %d", token, resp.StatusCode)
		}
	}
	// An agent without a secret is open, and one that does not exist is not found.
	mcpClient(t, ts.URL+"/open/mcp", nil, nil)
	if resp, err := http.Post(ts.URL+"/nope/mcp", "application/json", strings.NewReader("{}")); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent: %v %v", resp.StatusCode, err)
	}
}

func TestALongTaskIsReportedAndWaitedFor(t *testing.T) {
	progressEvery = 20 * time.Millisecond
	t.Cleanup(func() { progressEvery = time.Second })
	f := &fakeRunner{release: make(chan struct{})}
	ts := newTestServer(t, f, 1)
	var (
		mu    sync.Mutex
		notes []string
	)
	sess := mcpClient(t, ts.URL+"/pm/mcp", headers{"Authorization": "Bearer s3cret"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes = append(notes, req.Params.Message)
			mu.Unlock()
		},
	})

	// The task outlasts the wait (1s in these tests): the call says so, with its id.
	params := &mcp.CallToolParams{Name: "plan_project", Arguments: map[string]any{"message": "slow"}}
	params.SetProgressToken("p1")
	res, out := callTool(t, sess, params)
	if res.IsError || out.State != "working" || !strings.Contains(text(res), `get_task {"task_id": "`+out.TaskID+`"}`) || !strings.Contains(out.Next, "get_task") {
		t.Fatalf("result %q, %+v", text(res), out)
	}
	mu.Lock()
	gotProgress := slices.Contains(notes, "calling linear__get_issue")
	mu.Unlock()
	if !gotProgress {
		t.Errorf("progress notes = %q", notes)
	}

	// get_task waits for it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(f.release)
	}()
	res, out = callTool(t, sess, &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"task_id": out.TaskID}})
	if res.IsError || out.State != "completed" || text(res) != "echo: slow" {
		t.Errorf("get_task: %q, %+v", text(res), out)
	}
	if res, _ := callTool(t, sess, &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"task_id": "nope"}}); !res.IsError {
		t.Error("an unknown task should be an error")
	}
}

func TestATaskIsCanceledOverMCP(t *testing.T) {
	f := &fakeRunner{release: make(chan struct{})}
	defer close(f.release)
	ts := newTestServer(t, f, 1)
	sess := mcpClient(t, ts.URL+"/pm/mcp", headers{"Authorization": "Bearer s3cret"}, nil)
	_, out := callTool(t, sess, &mcp.CallToolParams{Name: "plan_project", Arguments: map[string]any{"message": "slow"}})
	res, out := callTool(t, sess, &mcp.CallToolParams{Name: "cancel_task", Arguments: map[string]any{"task_id": out.TaskID}})
	if !res.IsError || out.State != "canceled" || !strings.Contains(text(res), "canceled by the caller") {
		t.Errorf("cancel: %q, %+v", text(res), out)
	}
}

func TestAConversationContinuesOverMCP(t *testing.T) {
	f := &sessionRunner{}
	_, ts := newConvServer(t, f, 20, 1)
	sess := mcpClient(t, ts.URL+"/mem/mcp", nil, nil)
	res, first := callTool(t, sess, &mcp.CallToolParams{Name: "mem", Arguments: map[string]any{"message": "one"}})
	if res.IsError || !strings.Contains(text(res), "(context_id "+first.ContextID+":") {
		t.Fatalf("first: %q, %+v", text(res), first)
	}
	_, second := callTool(t, sess, &mcp.CallToolParams{Name: "mem", Arguments: map[string]any{"message": "two", "context_id": first.ContextID}})
	if second.ContextID != first.ContextID {
		t.Fatalf("context %q, want %q", second.ContextID, first.ContextID)
	}
	if job := f.job(1); job.Resume == "" || job.WorkDir != f.job(0).WorkDir {
		t.Errorf("the second task did not continue the first's session: %+v", job)
	}
}

func TestToolNames(t *testing.T) {
	got := toolNames([]config.Skill{{ID: "plan project"}, {ID: "get_task"}, {ID: "a"}, {ID: "a"}, {ID: "ünï"}})
	if strings.Join(got, ",") != "plan_project,get_task_2,a,a_2,_n_" {
		t.Errorf("names = %v", got)
	}
}
