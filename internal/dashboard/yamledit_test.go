package dashboard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `# The team.
listen: 127.0.0.1:7300
env_file: .env

cli:
  claude:
    command: claude   # on PATH

agents:
  # The planner.
  pm:
    description: Plans things,
      over two lines.
    secret: ${PM_SECRET}
    tools: [a, b]

  qa:
    description: Tests.
    instructions: |
      Be thorough.
      Report bugs.

# trailing comment
`

func mustPut(t *testing.T, text, section, oldKey, key string, v *yaml.Node) string {
	t.Helper()
	out, err := putEntry([]byte(text), section, oldKey, key, v)
	if err != nil {
		t.Fatal(err)
	}
	var check yaml.Node
	if err := yaml.Unmarshal(out, &check); err != nil {
		t.Fatalf("result is not YAML: %v\n%s", err, out)
	}
	return string(out)
}

func agentNode(desc string) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setScalar(m, "description", desc, strNode)
	return m
}

func TestReplacingAnEntryRewritesOnlyItsLines(t *testing.T) {
	_, pm, _ := strings.Cut(sample, "  pm:\n")
	pmBody, rest, _ := strings.Cut(pm, "\n\n  qa:")
	_ = pmBody
	got := mustPut(t, sample, "agents", "pm", "pm", agentNode("New."))
	want := strings.Replace(sample, "  pm:\n"+pmBody+"\n", "  pm:\n    description: New.\n", 1)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasSuffix(got, "\n\n  qa:"+rest) {
		t.Errorf("qa and the rest changed:\n%s", got)
	}
}

func TestRenameKeepsPositionAndComments(t *testing.T) {
	got := mustPut(t, sample, "agents", "pm", "planner", agentNode("New."))
	if !strings.Contains(got, "  # The planner.\n  planner:\n    description: New.\n\n  qa:") {
		t.Errorf("got:\n%s", got)
	}
}

func TestAddingAnEntryFollowsTheSectionsSpacing(t *testing.T) {
	got := mustPut(t, sample, "agents", "", "devops", agentNode("Deploys."))
	if !strings.Contains(got, "      Report bugs.\n\n  devops:\n    description: Deploys.\n\n# trailing comment\n") {
		t.Errorf("got:\n%s", got)
	}
	// One entry: no blank line between entries.
	got = mustPut(t, sample, "cli", "", "codex", &yaml.Node{Kind: yaml.MappingNode})
	if !strings.Contains(got, "    command: claude   # on PATH\n  codex: {}\n\nagents:") {
		t.Errorf("got:\n%s", got)
	}
}

func TestRemovingAnEntryLeavesNoGap(t *testing.T) {
	out, err := removeEntry([]byte(sample), "agents", "pm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "agents:\n  qa:\n") || strings.Contains(string(out), "planner") {
		t.Errorf("got:\n%s", out)
	}
	out, err = removeEntry([]byte(sample), "agents", "qa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    tools: [a, b]\n\n# trailing comment\n") {
		t.Errorf("got:\n%s", out)
	}
	out, err = removeEntry([]byte(sample), "", "env_file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# The team.\nlisten: 127.0.0.1:7300\n\ncli:") {
		t.Errorf("got:\n%s", out)
	}
}

func TestNewTopLevelKeysGoInOrder(t *testing.T) {
	got := mustPut(t, sample, "", "", "public_url", strNode("https://x.example"))
	if !strings.Contains(got, "listen: 127.0.0.1:7300\npublic_url: https://x.example\nenv_file: .env\n") {
		t.Errorf("got:\n%s", got)
	}
	got = mustPut(t, sample, "", "", "common_instructions", strNode("Line one.\nLine two.\n"))
	if !strings.Contains(got, "env_file: .env\ncommon_instructions: |\n  Line one.\n  Line two.\n\ncli:") {
		t.Errorf("got:\n%s", got)
	}
	got = mustPut(t, "agents:\n  pm:\n    description: x\n", "", "", "cli", &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{plainNode("claude"), {Kind: yaml.MappingNode}}})
	if got != "cli:\n  claude: {}\n\nagents:\n  pm:\n    description: x\n" {
		t.Errorf("got:\n%s", got)
	}
}

func TestSectionsThatAreNotBlockMappings(t *testing.T) {
	cases := map[string]string{
		"":                                 "agents:\n  pm:\n    description: New.\n",
		"# only a comment\n":               "# only a comment\n\nagents:\n  pm:\n    description: New.\n",
		"listen: x\nagents:\n":             "listen: x\nagents:\n  pm:\n    description: New.\n",
		"listen: x\nagents: {}\n":          "listen: x\nagents:\n  pm:\n    description: New.\n",
		"agents: {qa: {description: q}}\n": "agents:\n  qa: {description: q}\n  pm:\n    description: New.\n",
	}
	for in, want := range cases {
		if got := mustPut(t, in, "agents", "", "pm", agentNode("New.")); got != want {
			t.Errorf("from %q got:\n%s\nwant:\n%s", in, got, want)
		}
	}
}

func TestTheExampleConfigSurvivesAnEdit(t *testing.T) {
	raw, err := os.ReadFile("../../examples/delegent-team.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := entryValue(raw, "agents", "qa")
	if err != nil || v == nil {
		t.Fatal(v, err)
	}
	setScalar(v, "model", "opus", strNode)
	got := mustPut(t, string(raw), "agents", "qa", "qa", v)
	before, _, _ := strings.Cut(string(raw), "  qa:\n")
	_, after, _ := strings.Cut(string(raw), "\n  devops:\n")
	if !strings.HasPrefix(got, before) || !strings.HasSuffix(got, "\n  devops:\n"+after) {
		t.Errorf("lines outside qa changed:\n%s", got)
	}
	if !strings.Contains(got, "    model: opus\n") || !strings.Contains(got, "      remember: true # tasks in one Delegent run share a conversation\n") {
		t.Errorf("qa not edited as expected:\n%s", got)
	}
	var keys []string
	es, _ := entries([]byte(got), "agents")
	for _, e := range es {
		keys = append(keys, e.Key)
	}
	if strings.Join(keys, ",") != "pm,designer,engineer,qa,devops" {
		t.Errorf("agents: %v", keys)
	}
}
