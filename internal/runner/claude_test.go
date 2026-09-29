package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a script standing in for the claude CLI: it records its arguments and
// stdin next to itself, then prints the given stream-json lines and exits with code.
func fakeClaude(t *testing.T, code int, lines ...string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		`for a in "$@"; do printf '%s\0' "$a"; done > "` + dir + `/args"` + "\n" +
		`cat > "` + dir + `/stdin"` + "\n" +
		`cat "` + out + `"` + "\n" +
		`echo "boom on stderr" >&2` + "\n" +
		"exit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func recordedArgs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestClaudeRunsSealedAndParsesTheStream(t *testing.T) {
	bin, dir := fakeClaude(t, 0,
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__gw__linear__get_issue"}]}}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Found the story."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done: TES-1","num_turns":3,"total_cost_usd":0.02,"session_id":"s1","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":5}}`,
	)
	work := t.TempDir()
	var notes []string
	r := NewClaude(Options{Command: bin, Args: []string{"--extra"}})
	res, err := r.Run(context.Background(), Job{
		Prompt: "plan it", Instructions: "You are the PM.", Model: "haiku", MaxTurns: 12, WorkDir: work,
		MCP: []MCPServer{
			{Name: "gw", URL: "http://gw/mcp", Headers: map[string]string{"Authorization": "Bearer k", "X-Parent-Session": "sess_1"}, Tools: []string{"linear__get_issue", "linear__save_issue"}},
			{Name: "docs", URL: "http://docs/mcp"},
		},
	}, func(n string) { notes = append(notes, n) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Done: TES-1" || res.IsError || res.Turns != 3 || res.CostUSD != 0.02 || res.InputTokens != 100 || res.OutputTokens != 5 {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(notes, " | ") != "calling linear__get_issue | Found the story." {
		t.Errorf("progress = %q", notes)
	}

	args := recordedArgs(t, dir)
	for flag, want := range map[string]string{
		"--tools": "", "--setting-sources": "", "--permission-mode": "dontAsk", "--model": "haiku",
		"--max-turns": "12", "--append-system-prompt": "You are the PM.", "--output-format": "stream-json",
		"--allowedTools": "mcp__gw__linear__get_issue,mcp__gw__linear__save_issue,mcp__docs",
	} {
		if got, ok := flagValue(args, flag); !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", flag, got, ok, want)
		}
	}
	for _, flag := range []string{"-p", "--strict-mcp-config", "--no-session-persistence", "--verbose", "--extra"} {
		if !contains(args, flag) {
			t.Errorf("missing %s in %q", flag, args)
		}
	}
	if args[len(args)-2] != "--allowedTools" {
		t.Errorf("--allowedTools must come last (it takes a list): %q", args)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(b) != "plan it" {
		t.Errorf("prompt on stdin = %q", b)
	}
	cfgPath, _ := flagValue(args, "--mcp-config")
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatalf("mcp config %s: %v", cfgPath, err)
	}
	gw := cfg.MCPServers["gw"]
	if gw.Type != "http" || gw.URL != "http://gw/mcp" || gw.Headers["X-Parent-Session"] != "sess_1" || gw.Headers["Authorization"] != "Bearer k" {
		t.Errorf("gw config = %+v", gw)
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "null") {
		t.Errorf("the MCP config must not contain null values (Claude drops such a server):\n%s", raw)
	}
	if filepath.Dir(cfgPath) != work {
		t.Errorf("the MCP config belongs in the job's work dir, got %s", cfgPath)
	}
}

func TestClaudeGetsOnlyTheBuiltinToolsAJobLists(t *testing.T) {
	bin, dir := fakeClaude(t, 0,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"lspci -k","description":"list devices"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__gw__search","input":{"url":"https://secret.example"}}]}}`,
		`{"type":"result","subtype":"success","result":"An RTX 4090.","num_turns":2}`,
	)
	var notes []string
	r := NewClaude(Options{Command: bin})
	_, err := r.Run(context.Background(), Job{Prompt: "what is my gpu", WorkDir: t.TempDir(),
		BuiltinTools: []string{"Bash(lspci *)", "Read", "Bash(nvidia-smi)"},
		MCP:          []MCPServer{{Name: "gw", URL: "http://gw/mcp", Tools: []string{"search"}}},
	}, func(n string) { notes = append(notes, n) })
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgs(t, dir)
	if got, _ := flagValue(args, "--tools"); got != "Bash,Read" {
		t.Errorf("--tools = %q, want Bash,Read", got)
	}
	if got, _ := flagValue(args, "--allowedTools"); got != "mcp__gw__search,Bash(lspci *),Read,Bash(nvidia-smi)" {
		t.Errorf("--allowedTools = %q", got)
	}
	if strings.Join(notes, " | ") != "calling Bash: lspci -k | calling search" {
		t.Errorf("progress = %q", notes)
	}
}

func TestClaudeDefaultGivesEveryBuiltinTool(t *testing.T) {
	bin, dir := fakeClaude(t, 0, `{"type":"result","subtype":"success","result":"ok","num_turns":1}`)
	r := NewClaude(Options{Command: bin})
	_, err := r.Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir(), BuiltinTools: []string{AllBuiltinTools},
		MCP: []MCPServer{{Name: "gw", URL: "http://gw/mcp", Tools: []string{"search"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgs(t, dir)
	for flag, want := range map[string]string{"--tools": "default", "--permission-mode": "bypassPermissions", "--allowedTools": "mcp__gw__search"} {
		if got, _ := flagValue(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
}

func TestClaudeReportsAnUnfinishedRun(t *testing.T) {
	bin, _ := fakeClaude(t, 0, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":5}`)
	res, err := NewClaude(Options{Command: bin}).Run(context.Background(), Job{Prompt: "x", MaxTurns: 5, WorkDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "turn limit (5)") {
		t.Errorf("result = %+v", res)
	}
}

func TestClaudeWithoutAResultIsAnError(t *testing.T) {
	bin, _ := fakeClaude(t, 1, `{"type":"system","subtype":"init"}`)
	_, err := NewClaude(Options{Command: bin}).Run(context.Background(), Job{Prompt: "x", WorkDir: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "boom on stderr") {
		t.Errorf("want the CLI's stderr in the error, got %v", err)
	}
}

func TestClaudeIsStoppedWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewClaude(Options{Command: bin, KillGrace: time.Second}).Run(ctx, Job{Prompt: "x", WorkDir: t.TempDir()}, nil)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
}

func TestNewKnowsClaudeAndRejectsUnknownTypes(t *testing.T) {
	if _, err := New("claude", Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New("nope", Options{}); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("err = %v", err)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// A job that keeps its session saves it and resumes the one it names; one that does not
// keeps nothing.
func TestClaudeKeepsAndResumesSessions(t *testing.T) {
	ok := `{"type":"result","subtype":"success","is_error":false,"result":"hi","num_turns":1,"session_id":"s9"}`
	bin, dir := fakeClaude(t, 0, ok)
	c := NewClaude(Options{Command: bin})
	res, err := c.Run(context.Background(), Job{Prompt: "x", WorkDir: t.TempDir(), KeepSession: true, Resume: "s9"}, nil)
	if err != nil || res.SessionID != "s9" {
		t.Fatalf("%+v %v", res, err)
	}
	args := recordedArgs(t, dir)
	if v, _ := flagValue(args, "--resume"); v != "s9" {
		t.Errorf("--resume = %q in %v", v, args)
	}
	for _, a := range args {
		if a == "--no-session-persistence" {
			t.Error("a kept session was not saved")
		}
	}
	bin, dir = fakeClaude(t, 0, ok)
	if _, err := NewClaude(Options{Command: bin}).Run(context.Background(), Job{Prompt: "x", WorkDir: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	args = recordedArgs(t, dir)
	if _, has := flagValue(args, "--resume"); has || !strings.Contains(strings.Join(args, " "), "--no-session-persistence") {
		t.Errorf("a stateless job: %v", args)
	}
}

func TestClaudeReportsAMissingSession(t *testing.T) {
	bin, _ := fakeClaude(t, 1, `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"session_id":"gone","errors":["No conversation found with session ID: gone"]}`)
	_, err := NewClaude(Options{Command: bin}).Run(context.Background(), Job{Prompt: "x", WorkDir: t.TempDir(), KeepSession: true, Resume: "gone"}, nil)
	if err != ErrSessionNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestClaudeForgetsAWorkDirsSessions(t *testing.T) {
	config := t.TempDir()
	work := filepath.Join(t.TempDir(), "a2a-pm-ctx-123")
	os.MkdirAll(work, 0o700)
	project := filepath.Join(config, "projects", projectDirName(work))
	os.MkdirAll(project, 0o700)
	os.WriteFile(filepath.Join(project, "s1.jsonl"), []byte("{}"), 0o600)
	keep := filepath.Join(config, "projects", "-home-someone-else")
	os.MkdirAll(keep, 0o700)
	c := NewClaude(Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": config}}).(*Claude)
	if err := c.ForgetSessions(work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(project); !os.IsNotExist(err) {
		t.Error("the work dir's sessions are still there")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("another directory's sessions were deleted")
	}
}
