package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

func init() { Register("codex", NewCodex) }

// Codex runs tasks on the Codex CLI in headless mode (codex exec --json), under whatever login
// the CLI already has. Each run is sealed off like a Claude run: no user config, rules, plugins
// or apps, no shell and nothing written (a read-only sandbox, with the shell features off), no
// web search, no saved session unless the job keeps it, and only the job's MCP servers, whose
// tools are called without asking. BuiltinTools [default] lifts the seal: every built-in tool,
// no sandbox, nothing asked.
//
// Codex has no permission rules like Claude's, so it takes no other BuiltinTools, and no turn
// limit, so the runner stops a run after MaxTurns tool calls.
type Codex struct {
	opts Options
}

// NewCodex builds the codex runner.
func NewCodex(o Options) Runner {
	if o.Command == "" {
		o.Command = "codex"
	}
	if o.KillGrace == 0 {
		o.KillGrace = 5 * time.Second
	}
	return &Codex{opts: o}
}

// codexSealedFeatures are switched off for a sealed run: everything that runs commands, reaches
// beyond the job's MCP servers, or starts other agents. Set with -c, which (unlike --disable)
// does not fail on a feature this version of codex does not have.
var codexSealedFeatures = []string{
	"shell_tool", "unified_exec", "plugins", "remote_plugin", "apps", "browser_use",
	"browser_use_external", "computer_use", "in_app_browser", "image_generation", "view_image",
	"multi_agent", "multi_agent_v2", "tool_suggest", "goals",
}

// Run implements Runner.
func (c *Codex) Run(ctx context.Context, job Job, progress Progress) (Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	args, err := c.args(job)
	if err != nil {
		return Result{}, err
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(runCtx, c.opts.Command, args...)
	cmd.Dir = job.WorkDir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = os.Environ()
	for k, v := range c.opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	setProcessGroup(cmd)
	cmd.WaitDelay = c.opts.KillGrace
	stderr := &tailWriter{keep: 64 << 10} // codex logs a lot; the error is at the end
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting %s: %w", c.opts.Command, err)
	}
	res, gotResult, overLimit := c.read(stdout, job, progress, stop)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if overLimit {
		return Result{IsError: true, Text: describeFailure("error_max_turns", job.MaxTurns), Turns: res.Turns, SessionID: res.SessionID,
			InputTokens: res.InputTokens, OutputTokens: res.OutputTokens}, nil
	}
	if gotResult {
		return res, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if job.Resume != "" && strings.Contains(msg, "no rollout found") {
		return Result{}, ErrSessionNotFound
	}
	if msg == "" && waitErr != nil {
		msg = waitErr.Error()
	}
	if msg == "" {
		msg = "no result"
	}
	return Result{}, fmt.Errorf("%s ended without a result: %s", c.opts.Command, lastLines(msg, 5))
}

// args builds the command line for a job. The prompt comes on stdin ("-").
func (c *Codex) args(job Job) ([]string, error) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules", "-C", job.WorkDir}
	if !job.KeepSession {
		args = append(args, "--ephemeral")
	}
	switch {
	case len(job.BuiltinTools) == 0:
		args = append(args, "-s", "read-only", "-c", `approval_policy="never"`, "-c", `web_search="disabled"`)
		for _, f := range codexSealedFeatures {
			args = append(args, "-c", "features."+f+"=false")
		}
	case slices.Equal(job.BuiltinTools, []string{AllBuiltinTools}):
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		for _, f := range []string{"plugins", "remote_plugin", "apps"} { // still only the job's MCP servers
			args = append(args, "-c", "features."+f+"=false")
		}
	default:
		return nil, CheckBuiltinTools(c, job.BuiltinTools) // codex has no permission rules
	}
	if job.Model != "" {
		args = append(args, "-m", job.Model)
	}
	if job.Instructions != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(job.Instructions))
	}
	for _, s := range job.MCP {
		if !codexKey.MatchString(s.Name) {
			return nil, fmt.Errorf("mcp server name %q cannot be a codex config key", s.Name)
		}
		p := "mcp_servers." + s.Name + "."
		args = append(args, "-c", p+"url="+tomlString(s.URL), "-c", p+`default_tools_approval_mode="approve"`)
		if len(s.Tools) > 0 {
			args = append(args, "-c", p+"enabled_tools="+tomlArray(s.Tools))
		}
		if len(s.Headers) > 0 {
			args = append(args, "-c", p+"http_headers="+tomlTable(s.Headers))
		}
	}
	args = append(args, c.opts.Args...)
	if job.Resume != "" {
		args = append(args, "resume", job.Resume)
	}
	return append(args, "-"), nil
}

var codexKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// codexModel is an entry of codex's model catalog.
type codexModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	Priority    int    `json:"priority"`
}

// Models implements ModelLister: the models codex lists for its login (codex debug models),
// in its own order, leaving out those it hides.
func (c *Codex) Models(ctx context.Context) ([]Model, error) {
	cmd := exec.CommandContext(ctx, c.opts.Command, "debug", "models")
	cmd.Env = os.Environ()
	for k, v := range c.opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s debug models: %w", c.opts.Command, err)
	}
	var catalog struct {
		Models []codexModel `json:"models"`
	}
	if err := json.Unmarshal(out, &catalog); err != nil {
		return nil, fmt.Errorf("%s debug models: %w", c.opts.Command, err)
	}
	slices.SortStableFunc(catalog.Models, func(a, b codexModel) int { return a.Priority - b.Priority })
	var models []Model
	for _, m := range catalog.Models {
		if m.Visibility != "hide" && m.Slug != "" {
			models = append(models, Model{ID: m.Slug, Name: m.DisplayName, Description: m.Description})
		}
	}
	return models, nil
}

// codexEvent is the part of codex exec's JSONL output this runner reads.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Query   string `json:"query"`
	} `json:"item"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// read follows the event stream, reporting progress, until codex exits. A run that makes more
// than job.MaxTurns tool calls is stopped (stop kills it) and reported over the limit.
func (c *Codex) read(r io.Reader, job Job, progress Progress, stop func()) (res Result, got, overLimit bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	calls := 0
	for sc.Scan() {
		var ev codexEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			res.SessionID = ev.ThreadID
		case "item.started":
			note := ""
			switch ev.Item.Type {
			case "command_execution":
				note = "calling shell: " + shellCommand(ev.Item.Command)
			case "mcp_tool_call":
				note = "calling " + ev.Item.Tool
			case "web_search":
				note = "calling web_search: " + ev.Item.Query
			default:
				continue
			}
			calls++
			if job.MaxTurns > 0 && calls > job.MaxTurns {
				overLimit = true
				res.Turns = job.MaxTurns
				stop()
				continue
			}
			progress(note)
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				if t := strings.TrimSpace(ev.Item.Text); t != "" {
					res.Text = t
					progress(t)
				}
			}
		case "turn.completed":
			got = true
			res.InputTokens += ev.Usage.InputTokens
			res.OutputTokens += ev.Usage.OutputTokens
		case "turn.failed":
			got = true
			res.IsError = true
			res.Text = orText(ev.Error.Message, "the agent did not finish")
		case "error":
			got = true
			res.IsError = true
			res.Text = orText(ev.Message, "the agent did not finish")
		}
	}
	if !overLimit {
		res.Turns = calls + 1 // each tool call is followed by another step of the model
	}
	return res, got, overLimit
}

// shellCommand drops the shell codex wraps a command in ("/usr/bin/bash -lc lspci").
func shellCommand(cmd string) string {
	if _, after, ok := strings.Cut(cmd, " -lc "); ok {
		return after
	}
	return cmd
}

func orText(s, fallback string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return fallback
}

// ForgetSessions implements SessionForgetter: codex keeps each session as a rollout file under
// <CODEX_HOME>/sessions/YYYY/MM/DD/, whose first line records the directory it ran in. The
// rollouts of workDir are removed; no other.
func (c *Codex) ForgetSessions(workDir string) error {
	home := c.opts.Env["CODEX_HOME"]
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(h, ".codex")
	}
	dirs := map[string]bool{workDir: true}
	if abs, err := filepath.Abs(workDir); err == nil {
		dirs[abs] = true
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			dirs[real] = true
		}
	}
	root := filepath.Join(home, "sessions")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if dirs[rolloutDir(path)] {
			return os.Remove(path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// rolloutDir is the directory a rollout's session ran in, from its session_meta line.
func rolloutDir(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.Cwd
}

// TOML values for -c. A JSON string is a valid TOML basic string.

func tomlString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func tomlArray(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = tomlString(it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func tomlTable(m map[string]string) string {
	var pairs []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		pairs = append(pairs, tomlString(k)+" = "+tomlString(m[k]))
	}
	return "{ " + strings.Join(pairs, ", ") + " }"
}

// tailWriter keeps the last bytes written to it.
type tailWriter struct {
	buf  []byte
	keep int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.keep {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.keep:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	if len(t.buf) > t.keep {
		return string(t.buf[len(t.buf)-t.keep:])
	}
	return string(t.buf)
}
