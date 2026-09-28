package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() { Register("claude", NewClaude) }

// Claude runs tasks on Claude Code in headless mode (claude -p), under whatever login the
// CLI already has. Each run is sealed off: no built-in tools (no shell, files or web), no
// settings files, hooks or plugins, no saved session, and only the job's MCP servers, so
// everything the agent does goes through those servers.
type Claude struct {
	opts Options
}

// NewClaude builds the claude runner.
func NewClaude(o Options) Runner {
	if o.Command == "" {
		o.Command = "claude"
	}
	if o.KillGrace == 0 {
		o.KillGrace = 5 * time.Second
	}
	return &Claude{opts: o}
}

// mcpConfigFile is the --mcp-config file written into the job's work dir.
const mcpConfigFile = "mcp.json"

// Run implements Runner.
func (c *Claude) Run(ctx context.Context, job Job, progress Progress) (Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	args, err := c.args(job)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, c.opts.Command, args...)
	cmd.Dir = job.WorkDir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = os.Environ()
	for k, v := range c.opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	setProcessGroup(cmd)
	cmd.WaitDelay = c.opts.KillGrace
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, left: 64 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting %s: %w", c.opts.Command, err)
	}
	res, gotResult := c.read(stdout, job, progress)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if gotResult {
		return res, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" && waitErr != nil {
		msg = waitErr.Error()
	}
	if msg == "" {
		msg = "no result"
	}
	return Result{}, fmt.Errorf("%s ended without a result: %s", c.opts.Command, lastLines(msg, 5))
}

// args builds the command line for a job, writing its MCP config into the work dir.
func (c *Claude) args(job Job) ([]string, error) {
	servers := map[string]any{}
	var allowed []string
	for _, s := range job.MCP {
		server := map[string]any{"type": "http", "url": s.URL}
		if len(s.Headers) > 0 { // "headers": null makes Claude drop the whole server
			server["headers"] = s.Headers
		}
		servers[s.Name] = server
		if len(s.Tools) == 0 {
			allowed = append(allowed, "mcp__"+s.Name) // every tool of the server
		}
		for _, t := range s.Tools {
			allowed = append(allowed, "mcp__"+s.Name+"__"+t)
		}
	}
	cfg, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(job.WorkDir, mcpConfigFile)
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--tools", "", // no built-in tools: the agent acts only through its MCP servers
		"--mcp-config", cfgPath, "--strict-mcp-config",
		"--setting-sources", "", // ignore user, project and local settings (hooks, plugins)
		"--no-session-persistence",
		"--permission-mode", "dontAsk", // anything not allowed below is refused, never asked
	}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	if job.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(job.MaxTurns))
	}
	if job.Instructions != "" {
		args = append(args, "--append-system-prompt", job.Instructions)
	}
	args = append(args, c.opts.Args...)
	if len(allowed) > 0 {
		// Last, because the flag takes a list: nothing may follow it but its values.
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	return args, nil
}

// streamEvent is the part of claude's stream-json output this runner reads.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	SessionID    string  `json:"session_id"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// read follows the event stream, reporting progress, until the final result event.
func (c *Claude) read(r io.Reader, job Job, progress Progress) (Result, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var res Result
	got := false
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, part := range ev.Message.Content {
				switch part.Type {
				case "tool_use":
					progress("calling " + displayTool(part.Name, job.MCP))
				case "text":
					if t := strings.TrimSpace(part.Text); t != "" {
						progress(t)
					}
				}
			}
		case "result":
			got = true
			res = Result{
				Text:         ev.Result,
				IsError:      ev.IsError || ev.Subtype != "success",
				Turns:        ev.NumTurns,
				CostUSD:      ev.TotalCostUSD,
				InputTokens:  ev.Usage.InputTokens + ev.Usage.CacheCreationInputTokens + ev.Usage.CacheReadInputTokens,
				OutputTokens: ev.Usage.OutputTokens,
				SessionID:    ev.SessionID,
			}
			if res.IsError && res.Text == "" {
				res.Text = describeFailure(ev.Subtype, job.MaxTurns)
			}
		}
	}
	return res, got
}

// displayTool turns claude's mcp__<server>__<tool> back into the tool's own name.
func displayTool(name string, servers []MCPServer) string {
	for _, s := range servers {
		if p := "mcp__" + s.Name + "__"; strings.HasPrefix(name, p) {
			return strings.TrimPrefix(name, p)
		}
	}
	return name
}

func describeFailure(subtype string, maxTurns int) string {
	switch subtype {
	case "error_max_turns":
		return fmt.Sprintf("stopped after its turn limit (%d) before finishing", maxTurns)
	case "":
		return "the agent did not finish"
	}
	return "the agent did not finish (" + subtype + ")"
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// limitedWriter keeps the first `left` bytes and drops the rest.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left <= 0 {
		return n, nil
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	w, err := l.w.Write(p)
	l.left -= w
	if err != nil {
		return w, err
	}
	return n, nil
}
