package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExpandsEnvAndAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "# keys\nPM_KEY=dgk_abc\nexport PM_SECRET=\"s3cret\"\n")
	t.Setenv("GATEWAY_URL", "http://gw/mcp")
	p := write(t, dir, "agents.yaml", `
env_file: .env
cli:
  claude:
    command: /opt/claude/bin/claude
agents:
  pm:
    description: Plans things.
    instructions: Pricing is $9 a month.   # a bare $ is left alone; so is ${IN_A_COMMENT}
    secret: ${PM_SECRET}
    model: sonnet
    mcp:
      gateway:
        url: ${GATEWAY_URL}
        headers:
          Authorization: Bearer ${PM_KEY}
        forward_headers: [X-Parent-Session]
        tools: [linear__save_issue]
  qa:
    description: Tests things.
    cli: codex
    timeout: 5m
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.PublicURL != "http://"+DefaultListen {
		t.Errorf("listen %q, public %q", c.Listen, c.PublicURL)
	}
	pm := c.Agents["pm"]
	if pm.Name != "pm" || pm.CLI != "claude" || pm.Model != "sonnet" || pm.Secret != "s3cret" {
		t.Errorf("pm = %+v", pm)
	}
	if pm.Instructions != "Pricing is $9 a month." {
		t.Errorf("instructions = %q", pm.Instructions)
	}
	if got := pm.MCP["gateway"].Headers["Authorization"]; got != "Bearer dgk_abc" {
		t.Errorf("authorization = %q", got)
	}
	if pm.MCP["gateway"].URL != "http://gw/mcp" {
		t.Errorf("url = %q", pm.MCP["gateway"].URL)
	}
	qa := c.Agents["qa"]
	if qa.CLI != "codex" || qa.Model != "" || time.Duration(qa.Timeout) != 5*time.Minute || qa.MaxTurns != DefaultMaxTurns || qa.MaxParallel != 1 {
		t.Errorf("qa defaults: %+v", qa)
	}
	if c.CLI["claude"].Bin("claude") != "/opt/claude/bin/claude" || c.CLI["codex"].Bin("codex") != "codex" {
		t.Errorf("cli settings: %+v", c.CLI)
	}
	if qa.Context.Remember || time.Duration(qa.Context.IdleTimeout) != DefaultContextIdle || qa.Context.MaxTasks != DefaultContextTasks {
		t.Errorf("qa context defaults: %+v", qa.Context)
	}
	if len(qa.Skills) != 1 || qa.Skills[0].ID != "qa" {
		t.Errorf("qa gets one skill named after it, got %+v", qa.Skills)
	}
	if got := c.AgentNames(); strings.Join(got, ",") != "pm,qa" {
		t.Errorf("names = %v", got)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"not set in the environment": "agents:\n  pm:\n    description: x\n    secret: ${SURELY_NOT_SET_ANYWHERE}\n",
		"URL path":                   "agents:\n  Bad Name:\n    description: x\n",
		"description is required":    "agents:\n  pm: {}\n",
		"runner: is now cli:":        "agents:\n  pm:\n    description: x\n    runner: codex\n",
		"runners: is gone":           "runners:\n  claude: {model: haiku}\nagents:\n  pm:\n    description: x\n",
		"url is required":            "agents:\n  pm:\n    description: x\n    mcp:\n      gw: {}\n",
		"field modle not found":      "agents:\n  pm:\n    description: x\n    modle: haiku\n",
		"no agents":                  "listen: 127.0.0.1:1\n",
		"list it alone":              "agents:\n  pm:\n    description: x\n    builtin_tools: [default, Bash]\n",
	}
	for want, body := range cases {
		p := write(t, t.TempDir(), "agents.yaml", body)
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
}

func TestEnvFileDoesNotOverrideTheEnvironment(t *testing.T) {
	t.Setenv("KEEP_ME", "from-env")
	p := write(t, t.TempDir(), ".env", "KEEP_ME=from-file\nNEW_ONE='quoted'\n")
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("NEW_ONE") })
	if os.Getenv("KEEP_ME") != "from-env" || os.Getenv("NEW_ONE") != "quoted" {
		t.Errorf("KEEP_ME=%q NEW_ONE=%q", os.Getenv("KEEP_ME"), os.Getenv("NEW_ONE"))
	}
}

func TestParseAcceptsAConfigWithoutAgents(t *testing.T) {
	for _, body := range []string{"", "# nothing yet\n", "listen: 127.0.0.1:1\n", "agents:\n"} {
		c, err := Parse([]byte(body), t.TempDir())
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if len(c.Agents) != 0 {
			t.Errorf("%q: agents %v", body, c.Agents)
		}
	}
}
