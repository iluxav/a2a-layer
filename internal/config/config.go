// Package config reads the YAML file that defines the server and its agents.
//
// Values may reference the environment as ${NAME}; the file named by env_file is loaded first
// (without overriding variables already set), so secrets can live in a .env next to the
// config instead of in it. Only the ${NAME} form is expanded: a bare $ (as in "$9 a month")
// is left alone.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	// Listen is the address the HTTP server binds, e.g. 127.0.0.1:7300.
	Listen string `yaml:"listen"`
	// PublicURL is the base URL agents advertise in their cards (default http://<listen>).
	PublicURL string `yaml:"public_url"`
	// EnvFile is loaded into the environment before ${NAME} expansion, relative to the config.
	EnvFile string `yaml:"env_file"`
	// WorkDir holds each task's scratch directory (default: the system temp dir).
	WorkDir string `yaml:"work_dir"`
	// CommonInstructions are appended to every agent's instructions.
	CommonInstructions string `yaml:"common_instructions"`
	// Runners are the command-line agents tasks run on, by name.
	Runners map[string]Runner `yaml:"runners"`
	// Agents are served at /<name>.
	Agents map[string]*Agent `yaml:"agents"`
}

// Runner configures one command-line agent (a CLI such as claude) that tasks run on.
type Runner struct {
	// Type picks the implementation (claude). Default: the runner's own name.
	Type string `yaml:"type"`
	// Command is the executable. Default: the implementation's usual binary name.
	Command string `yaml:"command"`
	// Model, MaxTurns and Timeout are defaults an agent can override.
	Model    string   `yaml:"model"`
	MaxTurns int      `yaml:"max_turns"`
	Timeout  Duration `yaml:"timeout"`
	// Args are extra arguments added to every run.
	Args []string `yaml:"args"`
	// Env is added to the runner's environment.
	Env map[string]string `yaml:"env"`
}

// Agent is one A2A agent: its card, what runs it, and the MCP servers it may use.
type Agent struct {
	Name         string  `yaml:"-"`
	Description  string  `yaml:"description"`
	Version      string  `yaml:"version"`
	Instructions string  `yaml:"instructions"`
	Skills       []Skill `yaml:"skills"`

	// Runner names the runner (default: the only one configured).
	Runner   string   `yaml:"runner"`
	Model    string   `yaml:"model"`
	MaxTurns int      `yaml:"max_turns"`
	Timeout  Duration `yaml:"timeout"`
	// MaxParallel caps how many of this agent's tasks run at once; the rest wait (default 1).
	MaxParallel int `yaml:"max_parallel"`
	// Secret, when set, is the bearer token a caller must present.
	Secret string `yaml:"secret"`

	// MCP are the servers the agent may use, by name. Nothing else is available to it.
	MCP map[string]MCPServer `yaml:"mcp"`
}

// Skill is one entry of the agent card's skill list.
type Skill struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Examples    []string `yaml:"examples"`
}

// MCPServer is a Streamable HTTP MCP server an agent may use.
type MCPServer struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	// ForwardHeaders are copied from the A2A request that started a task onto every call the
	// task makes to this server (e.g. X-Delegent-Session, so a gateway can chain the hops).
	ForwardHeaders []string `yaml:"forward_headers"`
	// Tools limits the agent to these tools of the server. Empty allows all of them.
	Tools []string `yaml:"tools"`
}

// Duration is a time.Duration written as "15m" in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (e.g. 90s, 15m)", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// Defaults applied when neither the agent nor its runner says otherwise.
const (
	DefaultListen   = "127.0.0.1:7300"
	DefaultMaxTurns = 30
	DefaultTimeout  = 15 * time.Minute
	DefaultRunner   = "claude"
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	mcpPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envRef      = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// Load reads, expands, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// A first, unexpanded pass finds env_file, so it can feed the expansion.
	var pre struct {
		EnvFile string `yaml:"env_file"`
	}
	if err := yaml.Unmarshal(raw, &pre); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if pre.EnvFile != "" {
		envPath := pre.EnvFile
		if !filepath.IsAbs(envPath) {
			envPath = filepath.Join(filepath.Dir(path), envPath)
		}
		if err := LoadEnvFile(envPath); err != nil {
			return nil, err
		}
	}
	// Expand ${NAME} in values only (never in comments), then decode strictly so a
	// misspelled key is an error rather than silently ignored.
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := expandNode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.finish(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// expandNode expands ${NAME} in every scalar value of the document.
func expandNode(n *yaml.Node) error {
	var errs []error
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
			v, err := expand(n.Value)
			if err != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", n.Line, err))
			}
			n.Value = v
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return errors.Join(errs...)
}

// expand replaces every ${NAME} with its environment value; a name that is not set is an error.
func expand(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("not set in the environment or env_file: %s", strings.Join(uniq(missing), ", "))
	}
	return out, nil
}

// finish applies defaults and validates.
func (c *Config) finish(dir string) error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.PublicURL == "" {
		c.PublicURL = "http://" + c.Listen
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		c.WorkDir = filepath.Join(dir, c.WorkDir)
	}
	if len(c.Runners) == 0 {
		c.Runners = map[string]Runner{DefaultRunner: {}}
	}
	for name, r := range c.Runners {
		if r.Type == "" {
			r.Type = name
		}
		c.Runners[name] = r
	}
	if len(c.Agents) == 0 {
		return errors.New("no agents defined")
	}
	var errs []error
	for name, a := range c.Agents {
		if a == nil {
			a = &Agent{}
			c.Agents[name] = a
		}
		a.Name = name
		if err := c.finishAgent(a); err != nil {
			errs = append(errs, fmt.Errorf("agent %s: %w", name, err))
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}

func (c *Config) finishAgent(a *Agent) error {
	if !namePattern.MatchString(a.Name) {
		return errors.New("the name is its URL path: use lowercase letters, digits, - and _")
	}
	if a.Name == "mcp-proxy" {
		return errors.New("the name mcp-proxy is reserved (the tasks' MCP proxy lives at /mcp-proxy/)")
	}
	if a.Runner == "" {
		if len(c.Runners) != 1 {
			return errors.New("several runners are configured: say which one with runner:")
		}
		for name := range c.Runners {
			a.Runner = name
		}
	}
	r, ok := c.Runners[a.Runner]
	if !ok {
		return fmt.Errorf("runner %q is not configured", a.Runner)
	}
	if a.Model == "" {
		a.Model = r.Model
	}
	if a.MaxTurns == 0 {
		a.MaxTurns = r.MaxTurns
	}
	if a.MaxTurns == 0 {
		a.MaxTurns = DefaultMaxTurns
	}
	if a.Timeout == 0 {
		a.Timeout = r.Timeout
	}
	if a.Timeout == 0 {
		a.Timeout = Duration(DefaultTimeout)
	}
	if a.MaxParallel <= 0 {
		a.MaxParallel = 1
	}
	if a.Version == "" {
		a.Version = "1.0.0"
	}
	if a.Description == "" {
		return errors.New("description is required (it is what callers see on the card)")
	}
	if len(a.Skills) == 0 {
		a.Skills = []Skill{{ID: a.Name, Name: a.Name, Description: a.Description}}
	}
	for i, s := range a.Skills {
		if s.ID == "" || s.Description == "" {
			return fmt.Errorf("skill %d needs an id and a description", i+1)
		}
		if s.Name == "" {
			a.Skills[i].Name = s.ID
		}
	}
	for name, m := range a.MCP {
		if !mcpPattern.MatchString(name) {
			return fmt.Errorf("mcp server %q: use letters, digits, - and _ in the name", name)
		}
		if m.URL == "" {
			return fmt.Errorf("mcp server %q: url is required", name)
		}
	}
	return nil
}

// LoadEnvFile sets KEY=VALUE lines from path into the environment, leaving variables that are
// already set alone. Blank lines, # comments, an "export " prefix and quotes are understood.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("env_file %s does not exist (copy the example next to it and fill it in)", path)
	}
	if err != nil {
		return fmt.Errorf("env_file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// AgentNames lists the agents in a stable order.
func (c *Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
