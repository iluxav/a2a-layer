package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCodex writes a script standing in for the codex CLI: it records its arguments and stdin
// next to itself, prints the given JSONL lines, then writes stderr and exits with code.
func fakeCodex(t *testing.T, code int, stderr string, lines ...string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		`for a in "$@"; do printf '%s\0' "$a"; done > "` + dir + `/args"` + "\n" +
		`cat > "` + dir + `/stdin"` + "\n" +
		`cat "` + out + `"` + "\n" +
		`printf '%s' '` + stderr + `' >&2` + "\n" +
		"exit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestCodexRunsSealedAndParsesTheStream(t *testing.T) {
	bin, dir := fakeCodex(t, 0, "2026 INFO lots of logs\n",
		`{"type":"thread.started","thread_id":"th-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Looking it up.\n"}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"mcp_tool_call","server":"gw","tool":"linear__get_issue","arguments":{}}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"gw","tool":"linear__get_issue","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"error","message":"a warning, not the answer"}}`,
		`not json at all`,
		`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Done: TES-1"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":90,"output_tokens":5}}`,
	)
	work := t.TempDir()
	var notes []string
	r := NewCodex(Options{Command: bin, Args: []string{"-c", `model_reasoning_effort="low"`}})
	res, err := r.Run(context.Background(), Job{
		Prompt: "plan it", Instructions: "You are the PM.\nSay \"hi\".", Model: "gpt-5", MaxTurns: 12, WorkDir: work,
		MCP: []MCPServer{{Name: "gw", URL: "http://127.0.0.1:7300/mcp-proxy/t/gw", Tools: []string{"linear__get_issue"}}},
	}, func(n string) { notes = append(notes, n) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Done: TES-1" || res.IsError || res.Turns != 2 || res.InputTokens != 100 || res.OutputTokens != 5 || res.SessionID != "th-1" {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(notes, " | ") != "Looking it up. | calling linear__get_issue | Done: TES-1" {
		t.Errorf("progress = %q", notes)
	}
	args := recordedArgs(t, dir)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"exec --json --skip-git-repo-check --ignore-user-config --ignore-rules -C " + work + " --ephemeral",
		"-s read-only", `approval_policy="never"`, `web_search="disabled"`,
		"features.shell_tool=false", "features.unified_exec=false", "features.plugins=false", "features.apps=false",
		"-m gpt-5", `developer_instructions="You are the PM.\nSay \"hi\"."`,
		`mcp_servers.gw.url="http://127.0.0.1:7300/mcp-proxy/t/gw"`, `mcp_servers.gw.default_tools_approval_mode="approve"`,
		`mcp_servers.gw.enabled_tools=["linear__get_issue"]`, `model_reasoning_effort="low"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q:\n%s", want, joined)
		}
	}
	if args[len(args)-1] != "-" {
		t.Errorf("the prompt should come on stdin (-): %q", args)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(b) != "plan it" {
		t.Errorf("prompt on stdin = %q", b)
	}
}

func TestCodexBuiltinTools(t *testing.T) {
	bin, dir := fakeCodex(t, 0, "", `{"type":"turn.completed","usage":{}}`)
	r := NewCodex(Options{Command: bin})
	if _, err := r.Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir(), BuiltinTools: []string{AllBuiltinTools}}, nil); err != nil {
		t.Fatal(err)
	}
	args := recordedArgs(t, dir)
	if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || slices.Contains(args, "read-only") ||
		slices.Contains(args, "features.shell_tool=false") || !slices.Contains(args, "features.plugins=false") {
		t.Errorf("default should lift the seal but keep to the job's MCP servers: %q", args)
	}
	if _, err := r.Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir(), BuiltinTools: []string{"Bash(lspci *)"}}, nil); err == nil || !strings.Contains(err.Error(), "not permission rules") {
		t.Errorf("a permission rule should be refused, got %v", err)
	}
}

func TestCodexStopsAtItsTurnLimit(t *testing.T) {
	call := `{"type":"item.started","item":{"type":"command_execution","command":"/usr/bin/bash -lc lspci"}}`
	bin, _ := fakeCodex(t, 0, "", call, call, call, `{"type":"turn.completed","usage":{}}`)
	var notes []string
	res, err := NewCodex(Options{Command: bin}).Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir(), MaxTurns: 2}, func(n string) { notes = append(notes, n) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "turn limit (2)") || res.Turns != 2 {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(notes, " | ") != "calling shell: lspci | calling shell: lspci" {
		t.Errorf("progress = %q", notes)
	}
}

func TestCodexResumesAndReportsAMissingSession(t *testing.T) {
	bin, dir := fakeCodex(t, 1, "Error: thread/resume: thread/resume failed: no rollout found for thread id th-9 (code -32600)\n")
	_, err := NewCodex(Options{Command: bin}).Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir(), Resume: "th-9", KeepSession: true}, nil)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
	args := recordedArgs(t, dir)
	if strings.Join(args[len(args)-3:], " ") != "resume th-9 -" || slices.Contains(args, "--ephemeral") {
		t.Errorf("args = %q", args)
	}

	bin, _ = fakeCodex(t, 1, "Error: something else broke\n")
	if _, err := NewCodex(Options{Command: bin}).Run(context.Background(), Job{Prompt: "hi", WorkDir: t.TempDir()}, nil); err == nil || !strings.Contains(err.Error(), "something else broke") {
		t.Errorf("err = %v", err)
	}
}

func TestCodexForgetsAWorkDirsSessions(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "09", "28")
	os.MkdirAll(day, 0o700)
	mine := filepath.Join(day, "rollout-a.jsonl")
	other := filepath.Join(day, "rollout-b.jsonl")
	os.WriteFile(mine, []byte(`{"type":"session_meta","payload":{"id":"a","cwd":"`+work+`","base_instructions":"`+strings.Repeat("x", 100000)+`"}}`+"\n{}\n"), 0o600)
	os.WriteFile(other, []byte(`{"type":"session_meta","payload":{"id":"b","cwd":"/home/someone/project"}}`+"\n"), 0o600)
	r := NewCodex(Options{Env: map[string]string{"CODEX_HOME": home}}).(*Codex)
	if err := r.ForgetSessions(work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Error("the work dir's session is still there")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another directory's session was removed")
	}
	if err := NewCodex(Options{Env: map[string]string{"CODEX_HOME": t.TempDir()}}).(*Codex).ForgetSessions(work); err != nil {
		t.Errorf("no sessions dir yet: %v", err)
	}
}

func TestCodexIsStoppedWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewCodex(Options{Command: bin, KillGrace: time.Second}).Run(ctx, Job{Prompt: "hi", WorkDir: dir}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
}

func TestCodexListsItsModels(t *testing.T) {
	bin, dir := fakeCodex(t, 0, "", `{"models":[`+
		`{"slug":"gpt-b","display_name":"GPT-B","description":"Second.","visibility":"list","priority":2},`+
		`{"slug":"internal","display_name":"Internal","visibility":"hide","priority":0},`+
		`{"slug":"gpt-a","display_name":"GPT-A","description":"First.","visibility":"list","priority":1}]}`)
	models, err := NewCodex(Options{Command: bin}).(ModelLister).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != (Model{ID: "gpt-a", Name: "GPT-A", Description: "First."}) || models[1].ID != "gpt-b" {
		t.Errorf("models = %+v", models)
	}
	if args := recordedArgs(t, dir); strings.Join(args, " ") != "debug models" {
		t.Errorf("args = %q", args)
	}
}
