package dashboard

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/runner"
)

// echoRunner stands in for a CLI: it lists the tools of the job's MCP servers (reached through
// the dashboard's proxy), then answers "echo: <prompt>", once release is closed if set.
type echoRunner struct {
	mu      sync.Mutex
	tools   []string
	release chan struct{}
}

func (e *echoRunner) Run(ctx context.Context, job runner.Job, progress runner.Progress) (runner.Result, error) {
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
			e.mu.Lock()
			e.tools = append(e.tools, tool.Name)
			e.mu.Unlock()
		}
		sess.Close()
	}
	progress("thinking about " + job.Prompt)
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
	return runner.Result{Text: "echo: " + job.Prompt, Turns: 1, CostUSD: 0.002}, nil
}

// newDashboard serves a dashboard for a config file holding body ("" for no file) on a real
// listener, as the playground's MCP proxy needs.
func newDashboard(t *testing.T, body string, r runner.Runner) (*Dashboard, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{Path: path, BaseURL: "http://" + ln.Addr().String(),
		NewRunners: func(cfg *config.Config) (map[string]runner.Runner, error) {
			out := map[string]runner.Runner{}
			for name := range cfg.Runners {
				out[name] = r
			}
			return out, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(d.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(func() { d.Shutdown(); ts.Close() })
	return d, ts, path
}

func post(t *testing.T, ts *httptest.Server, path string, form url.Values) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func get(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnAgentIsCreatedEditedAndDeleted(t *testing.T) {
	_, ts, path := newDashboard(t, "", &echoRunner{})
	if !strings.Contains(get(t, ts, "/agents"), "No agents yet") {
		t.Error("an empty config should say there are no agents")
	}
	form := url.Values{
		"name": {"helper"}, "original": {""}, "description": {"Answers\r\nquestions."},
		"instructions": {"Be brief.  \r\nCite pages.\r\n"}, "max_turns": {"12"}, "timeout": {"5m"},
		"secret": {"${HELPER_SECRET}"}, "remember": {"true"},
		"mcp": {"r1"}, "mcp_r1_name": {"docs"}, "mcp_r1_url": {"https://docs.example.com/mcp"},
		"mcp_r1_headers": {"Authorization: Bearer ${DOCS_KEY}"}, "mcp_r1_tools": {"search, fetch"},
		"skill": {"s1"}, "skill_s1_id": {"answer"}, "skill_s1_description": {"Answers a question."},
		"skill_s1_examples": {"How do I rotate a key?\r\nWhat is A2A?"},
	}
	t.Setenv("HELPER_SECRET", "s3cret")
	t.Setenv("DOCS_KEY", "k")
	status, body, h := post(t, ts, "/agents", form)
	if status != http.StatusOK || !strings.Contains(h.Get("HX-Location"), "/agents?saved=helper") {
		t.Fatalf("save: %d %s %v", status, body, h)
	}
	want := `agents:
  helper:
    description: Answers questions.
    max_turns: 12
    timeout: 5m
    secret: ${HELPER_SECRET}
    context:
      remember: true
    skills:
      - id: answer
        description: Answers a question.
        examples: ['How do I rotate a key?', 'What is A2A?']
    instructions: |
      Be brief.
      Cite pages.
    mcp:
      docs:
        url: https://docs.example.com/mcp
        headers:
          Authorization: Bearer ${DOCS_KEY}
        tools: [search, fetch]
`
	if got := readFile(t, path); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("the written config does not load: %v", err)
	}

	// The edit form shows the values as written, ${NAME} and all.
	page := get(t, ts, "/agents/helper/edit")
	for _, s := range []string{`value="${HELPER_SECRET}"`, "Authorization: Bearer ${DOCS_KEY}", "Be brief.\nCite pages."} {
		if !strings.Contains(page, s) {
			t.Errorf("edit form lacks %q", s)
		}
	}

	// Renaming keeps its place; dropping a field removes it.
	form.Set("original", "helper")
	form.Set("name", "docs-helper")
	form.Set("max_turns", "")
	if status, body, _ := post(t, ts, "/agents", form); status != http.StatusOK || strings.Contains(body, "Not saved") {
		t.Fatalf("rename: %d %s", status, body)
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, "agents:\n  docs-helper:\n    description: Answers questions.\n    timeout: 5m\n") {
		t.Errorf("after rename:\n%s", got)
	}

	if status, _, h := post(t, ts, "/agents/docs-helper/delete", nil); status != http.StatusOK || !strings.Contains(h.Get("HX-Location"), "deleted=docs-helper") {
		t.Fatalf("delete: %d %v", status, h)
	}
	if got := readFile(t, path); got != "agents:\n" {
		t.Errorf("after delete: %q", got)
	}
}

func TestBuiltinToolsAreOptIn(t *testing.T) {
	_, ts, path := newDashboard(t, "", &echoRunner{})
	form := url.Values{"name": {"pc"}, "description": {"This machine."}, "builtin_tools": {"Bash(lspci *)\r\nRead\r\n"}}
	if _, page, _ := post(t, ts, "/agents", form); strings.Contains(page, "Not saved") {
		t.Fatalf("save:\n%s", page)
	}
	if got := readFile(t, path); got != "agents:\n  pc:\n    description: This machine.\n    builtin_tools: [Bash(lspci *), Read]\n" {
		t.Errorf("file:\n%s", got)
	}
	cfg, err := config.Load(path)
	if err != nil || strings.Join(cfg.Agents["pc"].BuiltinTools, ",") != "Bash(lspci *),Read" {
		t.Fatalf("load: %v %v", cfg, err)
	}
	if page := get(t, ts, "/agents"); !strings.Contains(page, "machine access") {
		t.Error("the list should flag an agent with built-in tools")
	}
	if page := get(t, ts, "/playground?agent=pc"); !strings.Contains(page, "Bash(lspci *), Read") {
		t.Error("the playground should list its built-in tools")
	}
}

func TestARunDoesNotRepeatItsAnswerInItsLog(t *testing.T) {
	r := &run{Started: time.Now(), done: true, ended: time.Now(),
		notes: []string{"started", "calling Bash: lspci", "You have an RTX 4090."}}
	r.task.Artifacts = []a2a.Artifact{{Parts: []a2a.Part{a2a.TextPart("You have an RTX 4090.\n")}}}
	if v := r.view(nil); strings.Join(v.Notes, "|") != "started|calling Bash: lspci" {
		t.Errorf("notes = %q", v.Notes)
	}
	r.notes = []string{"started", "A long answer, cut sho…"}
	r.task.Artifacts[0].Parts[0].Text = "A long answer, cut short in the notes."
	if v := r.view(nil); strings.Join(v.Notes, "|") != "started" {
		t.Errorf("notes = %q", v.Notes)
	}
}

func TestAChangeThatBreaksTheConfigIsNotSaved(t *testing.T) {
	const body = "# my agents\nagents:\n  pm:\n    description: Plans.   # keep me\n"
	_, ts, path := newDashboard(t, body, &echoRunner{})
	cases := map[string]url.Values{
		"lowercase letters":                        {"name": {"Bad Name"}, "description": {"x"}},
		"not a whole number":                       {"name": {"qa"}, "description": {"x"}, "max_turns": {"lots"}},
		"not a duration":                           {"name": {"qa"}, "description": {"x"}, "timeout": {"soon"}},
		"description is required":                  {"name": {"qa"}, "description": {""}},
		"NOPE_NOT_SET":                             {"name": {"qa"}, "description": {"x"}, "secret": {"${NOPE_NOT_SET}"}},
		"already an agent named pm":                {"name": {"pm"}, "original": {""}, "description": {"x"}},
		`runner &#34;codex&#34; is not configured`: {"name": {"qa"}, "description": {"x"}, "runner": {"codex"}},
	}
	for want, form := range cases {
		status, page, h := post(t, ts, "/agents", form)
		if status != http.StatusOK || h.Get("HX-Location") != "" || !strings.Contains(page, "Not saved") || !strings.Contains(page, want) {
			t.Errorf("%s: %d, HX-Location %q, page has error: %v", want, status, h.Get("HX-Location"), strings.Contains(page, want))
		}
	}
	if got := readFile(t, path); got != body {
		t.Errorf("the file changed:\n%s", got)
	}
}

func TestAProblemTheFileAlreadyHasDoesNotBlockOtherChanges(t *testing.T) {
	const body = "agents:\n  pm:\n    description: Plans.\n    secret: ${SURELY_UNSET_PM}\n"
	_, ts, path := newDashboard(t, body, &echoRunner{})
	if page := get(t, ts, "/agents"); !strings.Contains(page, "SURELY_UNSET_PM") {
		t.Error("the list should show the file's problem")
	}
	if _, page, _ := post(t, ts, "/agents", url.Values{"name": {"qa"}, "description": {"Tests."}}); strings.Contains(page, "Not saved") {
		t.Fatalf("an unrelated change was refused:\n%s", page)
	}
	if got := readFile(t, path); !strings.HasSuffix(got, "  qa:\n    description: Tests.\n") {
		t.Errorf("file:\n%s", got)
	}
}

func TestRunnersKeepEachAgentOnItsRunner(t *testing.T) {
	const body = "agents:\n  pm:\n    description: Plans.\n  qa:\n    description: Tests.\n"
	_, ts, path := newDashboard(t, body, &echoRunner{})
	if page := get(t, ts, "/runners"); !strings.Contains(page, "implicit") {
		t.Error("with no runners, the implicit default should be listed")
	}
	// A second runner: the implicit default is written beside it, and the agents keep it.
	if _, page, _ := post(t, ts, "/runners", url.Values{"name": {"fast"}, "type": {"claude"}, "model": {"haiku"}}); strings.Contains(page, "Not saved") {
		t.Fatalf("add runner:\n%s", page)
	}
	want := "runners:\n  claude: {}\n  fast:\n    type: claude\n    model: haiku\n\nagents:\n  pm:\n    description: Plans.\n    runner: claude\n  qa:\n    description: Tests.\n    runner: claude\n"
	if got := readFile(t, path); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
	// Renaming a runner renames it on its agents.
	if _, page, _ := post(t, ts, "/runners", url.Values{"name": {"cc"}, "original": {"claude"}, "type": {"claude"}}); strings.Contains(page, "Not saved") {
		t.Fatalf("rename runner:\n%s", page)
	}
	if got := readFile(t, path); strings.Count(got, "runner: cc\n") != 2 || !strings.Contains(got, "  cc:\n    type: claude\n") {
		t.Errorf("file:\n%s", got)
	}
	// A runner agents use cannot be deleted.
	if _, page, _ := post(t, ts, "/runners/cc/delete", nil); !strings.Contains(page, `runner &#34;cc&#34; is not configured`) {
		t.Errorf("deleting a used runner should be refused:\n%s", page)
	}
}

func TestSettingsKeepUnchangedValuesAsWritten(t *testing.T) {
	const body = "listen: 127.0.0.1:7300\ncommon_instructions: >\n  Folded\n  text.\n\nagents:\n  pm:\n    description: Plans.\n"
	_, ts, path := newDashboard(t, body, &echoRunner{})
	form := url.Values{"listen": {"127.0.0.1:7300"}, "public_url": {"https://agents.example.com"}, "common_instructions": {"Folded text.\n"}}
	if _, page, _ := post(t, ts, "/settings", form); strings.Contains(page, "Not saved") {
		t.Fatalf("save:\n%s", page)
	}
	want := "listen: 127.0.0.1:7300\npublic_url: https://agents.example.com\ncommon_instructions: >\n  Folded\n  text.\n\nagents:\n  pm:\n    description: Plans.\n"
	if got := readFile(t, path); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
}

// fakeServer is an MCP server with two tools.
func fakeServer(t *testing.T) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1"}, nil)
	for _, name := range []string{"search", "delete_everything"} {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

var runID = regexp.MustCompile(`id="run-([0-9a-f-]+)"`)

func TestThePlaygroundRunsAnAgent(t *testing.T) {
	r := &echoRunner{release: make(chan struct{})}
	body := "agents:\n  helper:\n    description: Helps.\n    secret: s3cret\n    mcp:\n      docs:\n        url: " + fakeServer(t) + "\n        tools: [search]\n"
	d, ts, path := newDashboard(t, body, r)

	page := get(t, ts, "/playground?agent=helper")
	if !strings.Contains(page, `<option value="helper" selected>`) {
		t.Fatalf("playground page:\n%s", page)
	}
	_, card, _ := post(t, ts, "/playground/runs", url.Values{"agent": {"helper"}, "message": {"hello"}})
	m := runID.FindStringSubmatch(card)
	if m == nil || !strings.Contains(card, `hx-trigger="every 1s"`) {
		t.Fatalf("run card:\n%s", card)
	}
	id := m[1]

	// While it runs, the config changes: the next run gets a new server, this one finishes on its own.
	os.WriteFile(path, []byte(body+"  other:\n    description: Other.\n"), 0o600)
	if _, err := d.playground(); err != nil {
		t.Fatal(err)
	}
	close(r.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		card = get(t, ts, "/playground/runs/"+id)
		if strings.Contains(card, "completed") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, s := range []string{"echo: hello", "thinking about hello", "earlier config", "$0.0020"} {
		if !strings.Contains(card, s) {
			t.Errorf("finished card lacks %q:\n%s", s, card)
		}
	}
	if strings.Contains(card, "hx-trigger") {
		t.Error("a finished run should stop polling")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Join(r.tools, ",") != "search" {
		t.Errorf("the runner saw tools %v through the proxy, want only search", r.tools)
	}
}

func TestThePlaygroundReportsWhatStopsARun(t *testing.T) {
	_, ts, _ := newDashboard(t, "agents:\n  helper:\n    description: Helps.\n", &echoRunner{})
	for want, form := range map[string]url.Values{
		"no agent":           {"agent": {"nobody"}, "message": {"hi"}},
		"Write a message":    {"agent": {"helper"}, "message": {"  "}},
		"is not Name: value": {"agent": {"helper"}, "message": {"hi"}, "headers": {"garbage"}},
	} {
		if _, card, _ := post(t, ts, "/playground/runs", form); !strings.Contains(card, "not sent") || !strings.Contains(card, want) {
			t.Errorf("want %q in:\n%s", want, card)
		}
	}
}

func TestOtherSitesAreKeptOut(t *testing.T) {
	_, ts, _ := newDashboard(t, "", &echoRunner{})
	req, _ := http.NewRequest("GET", ts.URL+"/agents", nil)
	req.Host = "attacker.example:7301"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host header should be refused: %v %v", resp.StatusCode, err)
	}
	req, _ = http.NewRequest("POST", ts.URL+"/agents", strings.NewReader("name=x&description=y"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site POST should be refused: %v %v", resp.StatusCode, err)
	}
}
