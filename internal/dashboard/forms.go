package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The forms show and take values as the file writes them, before ${NAME} expansion, so a
// secret written as ${PM_SECRET} stays a reference.

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// agentForm is one agent as its form edits it.
type agentForm struct {
	Name, Original                                        string // Original is the name it was loaded under ("" for a new agent)
	Description, Version, Instructions                    string
	Runner, Model, MaxTurns, Timeout, MaxParallel, Secret string
	Remember                                              bool
	IdleTimeout, MaxTasks                                 string
	BuiltinTools                                          string // one permission rule per line
	MCP                                                   []mcpForm
	Skills                                                []skillForm
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

type runnerForm struct {
	Name, Original, Type, Command, Model, MaxTurns, Timeout string
	Args                                                    string // one per line
	Env                                                     string // KEY=VALUE lines
}

type settingsForm struct {
	Listen, PublicURL, EnvFile, WorkDir, CommonInstructions string
}

// The raw* types decode an entry with every scalar as written, so "30" and "${TURNS}" both fit.

type rawAgent struct {
	Description  string     `yaml:"description"`
	Version      string     `yaml:"version"`
	Instructions string     `yaml:"instructions"`
	Skills       []rawSkill `yaml:"skills"`
	Runner       string     `yaml:"runner"`
	Model        string     `yaml:"model"`
	MaxTurns     string     `yaml:"max_turns"`
	Timeout      string     `yaml:"timeout"`
	MaxParallel  string     `yaml:"max_parallel"`
	Secret       string     `yaml:"secret"`
	BuiltinTools []string   `yaml:"builtin_tools"`
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

type rawRunner struct {
	Type     string            `yaml:"type"`
	Command  string            `yaml:"command"`
	Model    string            `yaml:"model"`
	MaxTurns string            `yaml:"max_turns"`
	Timeout  string            `yaml:"timeout"`
	Args     []string          `yaml:"args"`
	Env      map[string]string `yaml:"env"`
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
	f.Runner, f.Model, f.MaxTurns, f.Timeout, f.MaxParallel, f.Secret = a.Runner, a.Model, a.MaxTurns, a.Timeout, a.MaxParallel, a.Secret
	f.Remember = a.Context.Remember == "true"
	f.IdleTimeout, f.MaxTasks = a.Context.IdleTimeout, a.Context.MaxTasks
	f.BuiltinTools = strings.Join(a.BuiltinTools, "\n")
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
		Runner:       strings.TrimSpace(v.Get("runner")), Model: strings.TrimSpace(v.Get("model")),
		MaxTurns: strings.TrimSpace(v.Get("max_turns")), Timeout: strings.TrimSpace(v.Get("timeout")),
		MaxParallel: strings.TrimSpace(v.Get("max_parallel")), Secret: strings.TrimSpace(v.Get("secret")),
		Remember:    v.Get("remember") == "true",
		IdleTimeout: strings.TrimSpace(v.Get("idle_timeout")), MaxTasks: strings.TrimSpace(v.Get("max_tasks")),
		BuiltinTools: block(v.Get("builtin_tools")),
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
	setScalar(m, "runner", f.Runner, strNode)
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
	setList(m, "builtin_tools", lines(f.BuiltinTools))

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

func runnerFromNode(name string, n *yaml.Node) (runnerForm, error) {
	f := runnerForm{Name: name, Original: name}
	if n == nil {
		return f, nil
	}
	var r rawRunner
	if err := n.Decode(&r); err != nil {
		return f, fmt.Errorf("runner %s: %w", name, err)
	}
	f.Type, f.Command, f.Model, f.MaxTurns, f.Timeout = r.Type, r.Command, r.Model, r.MaxTurns, r.Timeout
	f.Args = strings.Join(r.Args, "\n")
	var env []string
	for _, k := range sortedKeys(r.Env) {
		env = append(env, k+"="+r.Env[k])
	}
	f.Env = strings.Join(env, "\n")
	return f, nil
}

func runnerFromForm(v url.Values) (runnerForm, error) {
	f := runnerForm{
		Name: strings.TrimSpace(v.Get("name")), Original: v.Get("original"),
		Type: strings.TrimSpace(v.Get("type")), Command: strings.TrimSpace(v.Get("command")),
		Model: strings.TrimSpace(v.Get("model")), MaxTurns: strings.TrimSpace(v.Get("max_turns")),
		Timeout: strings.TrimSpace(v.Get("timeout")), Args: block(v.Get("args")), Env: block(v.Get("env")),
	}
	var errs []string
	if !namePattern.MatchString(f.Name) {
		errs = append(errs, "name: use lowercase letters, digits, - and _")
	}
	errs = append(errs, checkNumber("max turns", f.MaxTurns)...)
	errs = append(errs, checkDuration("timeout", f.Timeout)...)
	if _, _, err := parsePairs(f.Env, "="); err != nil {
		errs = append(errs, "environment: "+err.Error())
	}
	return f, joinErrs(errs)
}

func (f runnerForm) node(old *yaml.Node) *yaml.Node {
	m := old
	if m == nil || m.Kind != yaml.MappingNode {
		m = &yaml.Node{Kind: yaml.MappingNode}
	}
	m.Style = 0
	typ := f.Type
	if typ == f.Name {
		typ = "" // the default
	}
	setScalar(m, "type", typ, strNode)
	setScalar(m, "command", f.Command, strNode)
	setScalar(m, "model", f.Model, strNode)
	setScalar(m, "max_turns", f.MaxTurns, plainNode)
	setScalar(m, "timeout", f.Timeout, strNode)
	setList(m, "args", lines(f.Args))
	keys, values, _ := parsePairs(f.Env, "=")
	setMap(m, "env", keys, values)
	return m
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
	return settingsForm(s), err
}

func settingsFromForm(v url.Values) settingsForm {
	return settingsForm{
		Listen: strings.TrimSpace(v.Get("listen")), PublicURL: strings.TrimSpace(v.Get("public_url")),
		EnvFile: strings.TrimSpace(v.Get("env_file")), WorkDir: strings.TrimSpace(v.Get("work_dir")),
		CommonInstructions: block(v.Get("common_instructions")),
	}
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
