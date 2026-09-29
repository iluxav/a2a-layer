package dashboard

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/runner"
)

// The forms show and take values as the file writes them, before ${NAME} expansion, so a
// secret written as ${PM_SECRET} stays a reference.

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// agentForm is one agent as its form edits it.
type agentForm struct {
	Name, Original                                     string // Original is the name it was loaded under ("" for a new agent)
	Description, Version, Instructions                 string
	CLI, Model, MaxTurns, Timeout, MaxParallel, Secret string
	Remember                                           bool
	IdleTimeout, MaxTasks                              string
	Builtin                                            builtinForm
	MCP                                                []mcpForm
	Skills                                             []skillForm
}

// mcpForm is one MCP server of an agent. Headers are "Name: value" lines; ForwardHeaders and
// Tools are lists, one per line or comma-separated.
type mcpForm struct {
	Row, Original, Name, URL, Headers, ForwardHeaders, Tools string
}

// skillForm is one skill of an agent's card. Tags are comma-separated; Examples one per line.
type skillForm struct {
	Row, ID, Name, Description, Tags, Examples string
}

// builtinForm is a choice of built-in tools: "none" (sealed; not written), "all" ([default])
// or "rules" (Rules, one per line).
type builtinForm struct {
	Mode, Rules string
}

type settingsForm struct {
	Listen, PublicURL, EnvFile, WorkDir, CommonInstructions string
	CLI                                                     []cliForm // one per CLI a2a-layer runs
}

// cliForm is how one CLI is run: its entry of the cli section. All empty, it runs as its own
// command on PATH.
type cliForm struct {
	Name, Command string
	Args          string // one per line
	Env           string // KEY=VALUE lines
	Bin           string // what runs: Command, or the CLI's name
	Found         bool   // Bin is on PATH
}

// The raw* types decode an entry with every scalar as written, so "30" and "${TURNS}" both fit.

type rawAgent struct {
	Description  string     `yaml:"description"`
	Version      string     `yaml:"version"`
	Instructions string     `yaml:"instructions"`
	Skills       []rawSkill `yaml:"skills"`
	CLI          string     `yaml:"cli"`
	Model        string     `yaml:"model"`
	MaxTurns     string     `yaml:"max_turns"`
	Timeout      string     `yaml:"timeout"`
	MaxParallel  string     `yaml:"max_parallel"`
	Secret       string     `yaml:"secret"`
	Context      struct {
		Remember    string `yaml:"remember"`
		IdleTimeout string `yaml:"idle_timeout"`
		MaxTasks    string `yaml:"max_tasks"`
	} `yaml:"context"`
}

type rawSkill struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Examples    []string `yaml:"examples"`
}

type rawMCP struct {
	URL            string            `yaml:"url"`
	Headers        map[string]string `yaml:"headers"`
	ForwardHeaders []string          `yaml:"forward_headers"`
	Tools          []string          `yaml:"tools"`
}

type rawCLI struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// agentFromNode reads an agent's entry into its form.
func agentFromNode(name string, n *yaml.Node) (agentForm, error) {
	f := agentForm{Name: name, Original: name}
	if n == nil {
		return f, nil
	}
	var a rawAgent
	if err := n.Decode(&a); err != nil {
		return f, fmt.Errorf("agent %s: %w", name, err)
	}
	f.Description, f.Version, f.Instructions = a.Description, a.Version, a.Instructions
	f.CLI, f.Model, f.MaxTurns, f.Timeout, f.MaxParallel, f.Secret = a.CLI, a.Model, a.MaxTurns, a.Timeout, a.MaxParallel, a.Secret
	f.Remember = a.Context.Remember == "true"
	f.IdleTimeout, f.MaxTasks = a.Context.IdleTimeout, a.Context.MaxTasks
	f.Builtin = builtinFromNode(n)
	for _, s := range a.Skills {
		f.Skills = append(f.Skills, skillForm{Row: newRow(), ID: s.ID, Name: s.Name, Description: s.Description,
			Tags: strings.Join(s.Tags, ", "), Examples: strings.Join(s.Examples, "\n")})
	}
	// The servers in file order, which a map would lose.
	if _, servers := lookup(n, "mcp"); servers != nil && servers.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(servers.Content); i += 2 {
			sname := servers.Content[i].Value
			var m rawMCP
			if err := servers.Content[i+1].Decode(&m); err != nil {
				return f, fmt.Errorf("agent %s: mcp server %s: %w", name, sname, err)
			}
			var headers []string
			for _, k := range sortedKeys(m.Headers) {
				headers = append(headers, k+": "+m.Headers[k])
			}
			f.MCP = append(f.MCP, mcpForm{Row: newRow(), Original: sname, Name: sname, URL: m.URL,
				Headers: strings.Join(headers, "\n"), ForwardHeaders: strings.Join(m.ForwardHeaders, ", "),
				Tools: strings.Join(m.Tools, "\n")})
		}
	}
	return f, nil
}

// agentFromForm reads a submitted agent form, checking what the config itself would not.
func agentFromForm(v url.Values) (agentForm, error) {
	f := agentForm{
		Name: strings.TrimSpace(v.Get("name")), Original: v.Get("original"),
		Description: oneLine(v.Get("description")), Version: strings.TrimSpace(v.Get("version")),
		Instructions: block(v.Get("instructions")),
		CLI:          strings.TrimSpace(v.Get("cli")), Model: strings.TrimSpace(v.Get("model")),
		MaxTurns: strings.TrimSpace(v.Get("max_turns")), Timeout: strings.TrimSpace(v.Get("timeout")),
		MaxParallel: strings.TrimSpace(v.Get("max_parallel")), Secret: strings.TrimSpace(v.Get("secret")),
		Remember:    v.Get("remember") == "true",
		IdleTimeout: strings.TrimSpace(v.Get("idle_timeout")), MaxTasks: strings.TrimSpace(v.Get("max_tasks")),
		Builtin: builtinFromForm(v),
	}
	for _, row := range v["mcp"] {
		p := "mcp_" + row + "_"
		m := mcpForm{Row: row, Original: v.Get(p + "orig"), Name: strings.TrimSpace(v.Get(p + "name")),
			URL: strings.TrimSpace(v.Get(p + "url")), Headers: block(v.Get(p + "headers")),
			ForwardHeaders: strings.TrimSpace(v.Get(p + "forward")), Tools: block(v.Get(p + "tools"))}
		if m.Name == "" && m.URL == "" && m.Tools == "" {
			continue // an empty row
		}
		f.MCP = append(f.MCP, m)
	}
	for _, row := range v["skill"] {
		p := "skill_" + row + "_"
		s := skillForm{Row: row, ID: strings.TrimSpace(v.Get(p + "id")), Name: strings.TrimSpace(v.Get(p + "name")),
			Description: oneLine(v.Get(p + "description")), Tags: strings.TrimSpace(v.Get(p + "tags")),
			Examples: block(v.Get(p + "examples"))}
		if s.ID == "" && s.Name == "" && s.Description == "" {
			continue
		}
		f.Skills = append(f.Skills, s)
	}

	var errs []string
	if !namePattern.MatchString(f.Name) {
		errs = append(errs, "name: the name is the agent's URL path, so use lowercase letters, digits, - and _")
	}
	if f.Name == "mcp-proxy" {
		errs = append(errs, "name: mcp-proxy is reserved")
	}
	errs = append(errs, checkNumber("max turns", f.MaxTurns)...)
	errs = append(errs, checkNumber("max parallel", f.MaxParallel)...)
	errs = append(errs, checkNumber("conversation max tasks", f.MaxTasks)...)
	errs = append(errs, checkDuration("timeout", f.Timeout)...)
	errs = append(errs, checkDuration("conversation idle timeout", f.IdleTimeout)...)
	errs = append(errs, f.Builtin.check()...)
	seen := map[string]bool{}
	for _, m := range f.MCP {
		if seen[m.Name] {
			errs = append(errs, "mcp server "+m.Name+" is listed twice")
		}
		seen[m.Name] = true
		if _, _, err := parsePairs(m.Headers, ":"); err != nil {
			errs = append(errs, "mcp server "+m.Name+": headers: "+err.Error())
		}
	}
	return f, joinErrs(errs)
}

// node merges the form into the agent's entry (nil for a new agent): the fields the form sets
// are written, anything else the entry has is kept.
func (f agentForm) node(old *yaml.Node) *yaml.Node {
	m := old
	if m == nil || m.Kind != yaml.MappingNode {
		m = &yaml.Node{Kind: yaml.MappingNode}
	}
	m.Style = 0
	setScalar(m, "description", f.Description, strNode)
	setScalar(m, "version", f.Version, strNode)
	if _, have := lookup(m, "cli"); have != nil || f.CLI != config.DefaultCLI {
		setScalar(m, "cli", f.CLI, strNode) // the default is written only where it already was
	}
	setScalar(m, "model", f.Model, strNode)
	setScalar(m, "max_turns", f.MaxTurns, plainNode)
	setScalar(m, "timeout", f.Timeout, strNode)
	setScalar(m, "max_parallel", f.MaxParallel, plainNode)
	setScalar(m, "secret", f.Secret, strNode)

	ctx := childMapping(m, "context")
	remember := ""
	if f.Remember {
		remember = "true"
	}
	setScalar(ctx, "remember", remember, plainNode)
	setScalar(ctx, "idle_timeout", f.IdleTimeout, strNode)
	setScalar(ctx, "max_tasks", f.MaxTasks, plainNode)
	setChild(m, "context", ctx)

	f.setSkills(m)
	setScalar(m, "instructions", f.Instructions, strNode)
	f.Builtin.set(m)

	servers := childMapping(m, "mcp")
	merged := &yaml.Node{Kind: yaml.MappingNode}
	for _, s := range f.MCP {
		_, sn := lookup(servers, s.Original)
		if sn == nil || sn.Kind != yaml.MappingNode {
			sn = &yaml.Node{Kind: yaml.MappingNode}
		}
		sn.Style = 0
		setScalar(sn, "url", s.URL, strNode)
		keys, values, _ := parsePairs(s.Headers, ":")
		setMap(sn, "headers", keys, values)
		setList(sn, "forward_headers", splitList(s.ForwardHeaders))
		setList(sn, "tools", splitList(s.Tools))
		merged.Content = append(merged.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: s.Name}, sn)
	}
	setChild(m, "mcp", merged)
	return m
}

// setSkills writes the skills, keeping the entry's own list when they did not change.
func (f agentForm) setSkills(m *yaml.Node) {
	var want []rawSkill
	for _, s := range f.Skills {
		want = append(want, rawSkill{ID: s.ID, Name: s.Name, Description: s.Description,
			Tags: splitList(s.Tags), Examples: lines(s.Examples)})
	}
	_, old := lookup(m, "skills")
	var have []rawSkill
	if old != nil && old.Decode(&have) == nil && slices.EqualFunc(have, want, sameSkill) && len(want) > 0 {
		return
	}
	if len(want) == 0 {
		setKey(m, "skills", "skills", nil)
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, s := range want {
		sn := &yaml.Node{Kind: yaml.MappingNode}
		setScalar(sn, "id", s.ID, strNode)
		setScalar(sn, "name", s.Name, strNode)
		setScalar(sn, "description", s.Description, strNode)
		setList(sn, "tags", s.Tags)
		setList(sn, "examples", s.Examples)
		seq.Content = append(seq.Content, sn)
	}
	setKey(m, "skills", "skills", seq)
}

func sameSkill(a, b rawSkill) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Description == b.Description &&
		slices.Equal(a.Tags, b.Tags) && slices.Equal(a.Examples, b.Examples)
}

// builtinFromNode reads an agent's builtin_tools.
func builtinFromNode(n *yaml.Node) builtinForm {
	_, v := lookup(n, "builtin_tools")
	var tools []string
	if v == nil || v.Kind == yaml.ScalarNode || v.Decode(&tools) != nil || len(tools) == 0 {
		return builtinForm{Mode: "none"}
	}
	switch {
	case slices.Equal(tools, []string{"default"}):
		return builtinForm{Mode: "all"}
	}
	return builtinForm{Mode: "rules", Rules: strings.Join(tools, "\n")}
}

func builtinFromForm(v url.Values) builtinForm {
	b := builtinForm{Mode: v.Get("builtin_mode"), Rules: block(v.Get("builtin_tools"))}
	if !slices.Contains([]string{"none", "all", "rules"}, b.Mode) {
		b.Mode = "none"
		if b.Rules != "" {
			b.Mode = "rules"
		}
	}
	return b
}

// tools are the builtin_tools the choice writes (nil for none).
func (b builtinForm) tools() []string {
	switch b.Mode {
	case "all":
		return []string{"default"}
	case "rules":
		return lines(b.Rules)
	}
	return nil
}

func (b builtinForm) check() []string {
	if b.Mode == "rules" && len(lines(b.Rules)) == 0 {
		return []string{"built-in tools: list at least one rule, or pick another option"}
	}
	return nil
}

// set writes the choice into an entry, keeping its node when unchanged.
func (b builtinForm) set(m *yaml.Node) {
	const key = "builtin_tools"
	switch b.Mode {
	case "all":
		setList(m, key, []string{"default"})
	case "rules":
		setList(m, key, lines(b.Rules))
	default:
		setKey(m, key, key, nil)
	}
}

func settingsFromText(text []byte) (settingsForm, error) {
	var s struct {
		Listen             string `yaml:"listen"`
		PublicURL          string `yaml:"public_url"`
		EnvFile            string `yaml:"env_file"`
		WorkDir            string `yaml:"work_dir"`
		CommonInstructions string `yaml:"common_instructions"`
	}
	err := yaml.Unmarshal(text, &s)
	f := settingsForm{Listen: s.Listen, PublicURL: s.PublicURL, EnvFile: s.EnvFile, WorkDir: s.WorkDir, CommonInstructions: s.CommonInstructions}
	for _, name := range runner.Types() {
		f.CLI = append(f.CLI, cliSettingsIn(text, name).located())
	}
	return f, err
}

func settingsFromForm(v url.Values) (settingsForm, error) {
	f := settingsForm{
		Listen: strings.TrimSpace(v.Get("listen")), PublicURL: strings.TrimSpace(v.Get("public_url")),
		EnvFile: strings.TrimSpace(v.Get("env_file")), WorkDir: strings.TrimSpace(v.Get("work_dir")),
		CommonInstructions: block(v.Get("common_instructions")),
	}
	var errs []string
	for _, name := range runner.Types() {
		p := "cli_" + name + "_"
		c := cliForm{Name: name, Command: strings.TrimSpace(v.Get(p + "command")), Args: block(v.Get(p + "args")), Env: block(v.Get(p + "env"))}
		if _, _, err := parsePairs(c.Env, "="); err != nil {
			errs = append(errs, name+": environment: "+err.Error())
		}
		f.CLI = append(f.CLI, c.located())
	}
	return f, joinErrs(errs)
}

// putSettings writes the settings, leaving the ones that did not change as they are written.
func putSettings(text []byte, s settingsForm) ([]byte, error) {
	have, err := settingsFromText(text)
	if err != nil {
		return nil, err
	}
	old := have.values()
	for i, kv := range s.values() {
		key, val := kv[0], kv[1]
		switch {
		case val == block(old[i][1]):
			continue
		case val == "":
			text, err = removeEntry(text, "", key)
		default:
			text, err = putEntry(text, "", key, key, strNode(val))
		}
		if err != nil {
			return nil, err
		}
	}
	for _, c := range s.CLI {
		was := cliSettingsIn(text, c.Name)
		switch {
		case c.Command == was.Command && c.Args == block(was.Args) && c.Env == block(was.Env):
			continue
		case c.empty():
			text, err = removeEntry(text, "cli", c.Name)
		default:
			n, _ := entryValue(text, "cli", c.Name)
			text, err = putEntry(text, "cli", c.Name, c.Name, c.node(n))
		}
		if err != nil {
			return nil, err
		}
	}
	if es, _ := entries(text, "cli"); len(es) == 0 {
		return removeEntry(text, "", "cli") // nothing left in it
	}
	return text, nil
}

// cliSettingsIn reads a CLI's entry of the cli section.
func cliSettingsIn(text []byte, name string) cliForm {
	f := cliForm{Name: name}
	n, _ := entryValue(text, "cli", name)
	var c rawCLI
	if n == nil || n.Decode(&c) != nil {
		return f
	}
	f.Command, f.Args = c.Command, strings.Join(c.Args, "\n")
	var env []string
	for _, k := range sortedKeys(c.Env) {
		env = append(env, k+"="+c.Env[k])
	}
	f.Env = strings.Join(env, "\n")
	return f
}

// located fills in what the CLI runs as, and whether that is on PATH.
func (c cliForm) located() cliForm {
	c.Bin = cmp.Or(c.Command, c.Name)
	_, err := exec.LookPath(c.Bin)
	c.Found = err == nil
	return c
}

func (c cliForm) empty() bool {
	return c.Command == "" && len(lines(c.Args)) == 0 && len(lines(c.Env)) == 0
}

func (c cliForm) node(old *yaml.Node) *yaml.Node {
	m := old
	if m == nil || m.Kind != yaml.MappingNode {
		m = &yaml.Node{Kind: yaml.MappingNode}
	}
	m.Style = 0
	setScalar(m, "command", c.Command, strNode)
	setList(m, "args", lines(c.Args))
	keys, values, _ := parsePairs(c.Env, "=")
	setMap(m, "env", keys, values)
	return m
}

// values are the settings by key, in the order they go into the file.
func (s settingsForm) values() [][2]string {
	return [][2]string{{"listen", s.Listen}, {"public_url", s.PublicURL}, {"env_file", s.EnvFile},
		{"work_dir", s.WorkDir}, {"common_instructions", s.CommonInstructions}}
}

// Text helpers. Browsers send textareas with CRLF line ends.

// block normalizes a multi-line value: LF line ends, no trailing spaces (which would stop it
// being written as a | block), and a single final newline.
func block(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	ls := strings.Split(strings.Trim(s, "\n"), "\n")
	for i, l := range ls {
		ls[i] = strings.TrimRight(l, " \t\r")
	}
	s = strings.Trim(strings.Join(ls, "\n"), "\n")
	if strings.Contains(s, "\n") {
		s += "\n"
	}
	return s
}

// oneLine joins a value typed over several lines, as a description is.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// lines splits one item per line, dropping blank lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// splitList splits on newlines and commas.
func splitList(s string) []string {
	return lines(strings.ReplaceAll(s, ",", "\n"))
}

// parsePairs reads "key<sep>value" lines, keeping their order.
func parsePairs(s, sep string) (keys []string, values map[string]string, err error) {
	values = map[string]string{}
	for _, l := range lines(s) {
		k, v, ok := strings.Cut(l, sep)
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, nil, fmt.Errorf("%q is not %s", l, map[string]string{":": "Name: value", "=": "KEY=value"}[sep])
		}
		if _, dup := values[k]; !dup {
			keys = append(keys, k)
		}
		values[k] = v
	}
	return keys, values, nil
}

func checkNumber(field, s string) []string {
	if s == "" || strings.Contains(s, "${") {
		return nil
	}
	if n, err := strconv.Atoi(s); err != nil || n < 0 {
		return []string{field + ": " + strconv.Quote(s) + " is not a whole number"}
	}
	return nil
}

func checkDuration(field, s string) []string {
	if s == "" || strings.Contains(s, "${") {
		return nil
	}
	if _, err := time.ParseDuration(s); err != nil {
		return []string{field + ": " + strconv.Quote(s) + " is not a duration (e.g. 90s, 15m, 1h)"}
	}
	return nil
}

// formError is a submitted form's problems, one per line.
type formError struct{ problems []string }

func (e *formError) Error() string { return strings.Join(e.problems, "\n") }

func joinErrs(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return &formError{errs}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// newRow names a repeated form row (an MCP server, a skill) so its fields stay together.
func newRow() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
