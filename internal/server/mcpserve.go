package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
)

// Each agent is also served as a Streamable HTTP MCP server at /<agent>/mcp, for the clients
// that call MCP servers but not A2A agents (Claude Code, Codex, pi, …). Each skill is a tool:
// calling it starts a task, exactly as message/send does, and waits for it, but only up to
// cfg.MCPWait, since MCP clients time tool calls out and a task can take many minutes. A task
// still running then is reported by id, and get_task waits for it again.
//
// This is not the per-task proxy under /mcp-proxy/, which serves a task its own MCP servers.

// mcpSessionIdle ends an MCP session its client has not used for this long.
const mcpSessionIdle = time.Hour

// progressEvery is how often a waiting call checks its task for a new note to report.
var progressEvery = time.Second

// askInput is what a skill's tool takes.
type askInput struct {
	Message   string `json:"message" jsonschema:"the request: complete and self-contained, since the agent sees nothing else"`
	ContextID string `json:"context_id,omitempty" jsonschema:"continue the conversation of an earlier result by passing its context_id; omit to start a new one"`
}

// taskInput is what get_task and cancel_task take.
type taskInput struct {
	TaskID string `json:"task_id" jsonschema:"the task_id of an earlier result"`
}

// taskOutput is every tool result's structured content.
type taskOutput struct {
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id" jsonschema:"pass it back as context_id to continue this conversation"`
	State     string `json:"state" jsonschema:"submitted, working, completed, failed or canceled"`
	Answer    string `json:"answer,omitempty" jsonschema:"the agent's answer, once completed"`
	Status    string `json:"status,omitempty" jsonschema:"the agent's latest word on the task: progress, or why it failed"`
	// Next repeats the text result's advice for clients that show a model only the structured
	// result (Claude Code does).
	Next string `json:"next,omitempty" jsonschema:"what to do next, while the task is not finished"`
}

// mcpEndpointFor builds the agent's MCP server and the HTTP handler that serves it.
func (s *Server) mcpEndpointFor(a *agent) (*mcp.Server, http.Handler) {
	srv := mcp.NewServer(&mcp.Implementation{Name: a.cfg.Name, Version: a.cfg.Version}, &mcp.ServerOptions{Instructions: a.cfg.Description})
	names := toolNames(a.cfg.Skills)
	for i, sk := range a.cfg.Skills {
		mcp.AddTool(srv, &mcp.Tool{Name: names[i], Title: sk.Name, Description: s.toolDescription(a, sk)},
			func(ctx context.Context, req *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, taskOutput, error) {
				return s.mcpAsk(ctx, a, req, in)
			})
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "get_task", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: fmt.Sprintf("Waits for a task this agent is still working on (up to %s), then reports where it is: its answer once completed.", s.wait())},
		func(ctx context.Context, req *mcp.CallToolRequest, in taskInput) (*mcp.CallToolResult, taskOutput, error) {
			t := s.tasks.get(a.cfg.Name, in.TaskID)
			if t == nil {
				return nil, taskOutput{}, fmt.Errorf("no task %q (a finished task is kept for %s)", in.TaskID, finishedTTL)
			}
			return s.mcpWait(ctx, req, a, t)
		})
	notDestructive := false // stopping a task discards nothing
	mcp.AddTool(srv, &mcp.Tool{Name: "cancel_task", Annotations: &mcp.ToolAnnotations{DestructiveHint: &notDestructive, IdempotentHint: true},
		Description: "Stops a task this agent is still working on."},
		func(ctx context.Context, req *mcp.CallToolRequest, in taskInput) (*mcp.CallToolResult, taskOutput, error) {
			t := s.tasks.get(a.cfg.Name, in.TaskID)
			if t == nil {
				return nil, taskOutput{}, fmt.Errorf("no task %q", in.TaskID)
			}
			if t.setState(a2a.StateCanceled, "canceled by the caller") {
				t.cancel()
				s.log.Info("task canceled", "agent", a.cfg.Name, "task", t.id, "via", "mcp")
			}
			return mcpResult(a, t.snapshot(nil))
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		SessionTimeout: mcpSessionIdle,
		// The SDK refuses a request that reaches a loopback address under another host name
		// (DNS rebinding). Behind a reverse proxy serving public_url that is every request,
		// so the check stays on only when the agents are served under a loopback name.
		DisableLocalhostProtection: !isLoopback(publicHost(s.cfg.PublicURL)),
	})
	return srv, handler
}

// toolDescription is a skill's description, its examples, and how to use the tool.
func (s *Server) toolDescription(a *agent, sk config.Skill) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(sk.Description))
	if len(sk.Examples) > 0 {
		b.WriteString("\n\nExamples:")
		for _, e := range sk.Examples {
			b.WriteString("\n- " + e)
		}
	}
	fmt.Fprintf(&b, "\n\nStarts a task on the %s agent and waits for it up to %s. If it is still working then, "+
		"the result says so with its task_id: wait for it with get_task, rather than asking again.", a.cfg.Name, s.wait())
	if a.cfg.Context.Remember {
		b.WriteString(" The agent remembers a conversation: pass an earlier result's context_id to continue it.")
	}
	return b.String()
}

func (s *Server) wait() time.Duration {
	if s.cfg.MCPWait > 0 {
		return time.Duration(s.cfg.MCPWait)
	}
	return config.DefaultMCPWait
}

// mcpAsk starts a task from a tool call and waits for it.
func (s *Server) mcpAsk(ctx context.Context, a *agent, req *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, taskOutput, error) {
	prompt := strings.TrimSpace(in.Message)
	if prompt == "" {
		return nil, taskOutput{}, errors.New("message is empty: say what the agent should do")
	}
	msg := a2a.Message{Kind: "message", Role: "user", MessageID: newID(), ContextID: strings.TrimSpace(in.ContextID),
		Parts: []a2a.Part{a2a.TextPart(prompt)}}
	var inbound http.Header // for the agent's forward_headers
	if req.Extra != nil {
		inbound = req.Extra.Header
	}
	return s.mcpWait(ctx, req, a, s.start(a, msg, prompt, inbound))
}

// mcpWait waits for a task to finish, up to the wait limit, sending its progress notes to a
// client that asked for progress. The task goes on if the client hangs up.
func (s *Server) mcpWait(ctx context.Context, req *mcp.CallToolRequest, a *agent, t *task) (*mcp.CallToolResult, taskOutput, error) {
	limit := time.NewTimer(s.wait())
	defer limit.Stop()
	tick := time.NewTicker(progressEvery)
	defer tick.Stop()
	token := req.Params.GetProgressToken()
	lastNote, sent := "", 0
	for waiting := true; waiting; {
		select {
		case <-t.done:
			waiting = false
		case <-limit.C:
			waiting = false
		case <-ctx.Done():
			waiting = false
		case <-s.ctx.Done():
			waiting = false
		case <-tick.C:
			m := t.snapshot(nil).Status.Message
			if token == nil || m == nil || m.MessageID == lastNote {
				continue
			}
			lastNote = m.MessageID
			sent++
			req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(sent), Message: m.Text()})
		}
	}
	return mcpResult(a, t.snapshot(nil))
}

// mcpResult reports a task as a tool result: the answer, the failure, or that it is still
// working (with how to wait for it).
func mcpResult(a *agent, task a2a.Task) (*mcp.CallToolResult, taskOutput, error) {
	out := taskOutput{TaskID: task.ID, ContextID: task.ContextID, State: task.Status.State}
	if m := task.Status.Message; m != nil {
		out.Status = m.Text()
	}
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			out.Answer += p.Text
		}
	}
	res := &mcp.CallToolResult{}
	var text string
	switch task.Status.State {
	case a2a.StateCompleted:
		text = out.Answer
		if text == "" {
			text = "The agent finished without an answer."
		}
		out.Status = "" // its last note is usually the answer again
	case a2a.StateFailed, a2a.StateCanceled:
		res.IsError = true
		text = "The task " + task.Status.State
		if out.Status != "" {
			text += ": " + out.Status
		}
	default:
		out.Next = fmt.Sprintf("Still working. Wait for it with get_task {\"task_id\": %q}, rather than asking again.", task.ID)
		text = fmt.Sprintf("The agent is still working on it (task_id %s). Wait for it with get_task {\"task_id\": %q}, rather than asking again.", task.ID, task.ID)
		if out.Status != "" {
			text += "\nLatest: " + out.Status
		}
	}
	if a.cfg.Context.Remember && !res.IsError {
		text += "\n\n(context_id " + task.ContextID + ": pass it to continue this conversation)"
	}
	res.Content = []mcp.Content{&mcp.TextContent{Text: text}}
	return res, out, nil
}

var toolNameInvalid = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// toolNames names each skill's tool after its id, as far as MCP allows (letters, digits, _ - .),
// keeping clear of get_task, cancel_task and each other.
func toolNames(skills []config.Skill) []string {
	used := map[string]bool{"get_task": true, "cancel_task": true}
	out := make([]string, len(skills))
	for i, sk := range skills {
		base := toolNameInvalid.ReplaceAllString(sk.ID, "_")
		if len(base) > 120 {
			base = base[:120]
		}
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		used[name] = true
		out[i] = name
	}
	return out
}

// publicHost is the host agents are served under, from the public URL.
func publicHost(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
