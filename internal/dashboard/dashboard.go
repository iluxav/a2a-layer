// Package dashboard is a web UI for an a2a-layer config file: add, edit and remove agents and
// runners, change the settings, and try the agents in a playground. The pages are rendered on
// the server and driven by htmx; the templates, the stylesheet and htmx itself are embedded in
// the binary.
//
// Every change is written back to the file, rewriting only the entry it touches, and only if it
// adds no problem the file did not already have (an unset ${NAME}, a missing description, …).
package dashboard

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/mcpproxy"
	"github.com/iluxav/a2a-layer/internal/runner"
	"github.com/iluxav/a2a-layer/internal/server"
)

//go:embed templates static
var assets embed.FS

// Options configure a Dashboard.
type Options struct {
	// Path is the config file. It need not exist: the first change creates it.
	Path string
	// BaseURL is where a CLI on this machine reaches the dashboard's listener; the playground's
	// tasks reach their MCP servers through it.
	BaseURL string
	// AnyHost accepts requests for any Host. Otherwise only localhost and loopback addresses
	// are served, so a web page cannot reach the dashboard through a name it controls.
	AnyHost bool
	Log     *slog.Logger
	// NewRunners builds the playground's runners (default server.NewRunners).
	NewRunners func(*config.Config) (map[string]runner.Runner, error)
}

// Dashboard serves the UI for one config file.
type Dashboard struct {
	path       string
	anyHost    bool
	log        *slog.Logger
	proxy      *mcpproxy.Proxy
	newRunners func(*config.Config) (map[string]runner.Runner, error)
	pages      map[string]*template.Template

	edit sync.Mutex // one change to the file at a time

	mu         sync.Mutex
	modelCache map[string]listedModels // by runner type and command
	play       *playground             // built from the file as it was last read
	live       []*playground           // play, and replaced ones whose runs have not finished
	convs      []*conversation         // the playground's, most recently used first
}

// New builds the dashboard.
func New(o Options) (*Dashboard, error) {
	d := &Dashboard{path: o.Path, anyHost: o.AnyHost, log: o.Log, proxy: mcpproxy.New(o.BaseURL), newRunners: o.NewRunners, pages: map[string]*template.Template{}, modelCache: map[string]listedModels{}}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.newRunners == nil {
		d.newRunners = server.NewRunners
	}
	for _, page := range []string{"agents", "agent", "settings", "playground"} {
		t, err := template.New(page).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/parts.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		d.pages[page] = t
	}
	return d, nil
}

var funcs = template.FuncMap{
	"join": strings.Join,
}

// Handler serves the pages, their assets, and the playground tasks' MCP proxy.
func (d *Dashboard) Handler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.Handle(mcpproxy.PathPrefix, d.proxy)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/agents", http.StatusSeeOther) })

	mux.HandleFunc("GET /agents", d.agentsPage)
	mux.HandleFunc("GET /agents/new", d.agentPage)
	mux.HandleFunc("GET /agents/{name}/edit", d.agentPage)
	mux.HandleFunc("POST /agents", d.saveAgent)
	mux.HandleFunc("POST /agents/{name}/delete", d.deleteAgent)
	mux.HandleFunc("GET /agents/{name}/card", d.agentCard)
	mux.HandleFunc("GET /parts/mcp", d.part("mcp-row", func() any { return mcpForm{Row: newRow()} }))
	mux.HandleFunc("GET /parts/cli", d.cliPart)
	mux.HandleFunc("GET /parts/skill", d.part("skill-row", func() any { return skillForm{Row: newRow()} }))

	mux.HandleFunc("GET /settings", d.settingsPage)
	mux.HandleFunc("POST /settings", d.saveSettings)

	mux.HandleFunc("GET /playground", d.playgroundPage)
	mux.HandleFunc("GET /playground/agent", d.playgroundAgent)
	mux.HandleFunc("GET /playground/conversations", d.conversationList)
	mux.HandleFunc("POST /playground/runs", d.newRun)
	mux.HandleFunc("GET /playground/runs/{id}", d.showRun)
	mux.HandleFunc("POST /playground/runs/{id}/cancel", d.cancelRunHandler)
	return d.guard(mux)
}

// guard keeps other web pages out: the dashboard changes the config and runs agents, and has
// no login. Cross-origin writes are refused, and so are requests for a host name other than
// localhost (a page could otherwise point a name it controls at 127.0.0.1).
func (d *Dashboard) guard(next http.Handler) http.Handler {
	h := http.NewCrossOriginProtection().Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.anyHost && !isLoopbackHost(r.Host) {
			http.Error(w, "the dashboard answers only on localhost", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// The config file.

// read returns the file's text; a file that does not exist yet reads as empty.
func (d *Dashboard) read() ([]byte, error) {
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (d *Dashboard) parse(text []byte) (*config.Config, error) {
	return config.Parse(text, filepath.Dir(d.path))
}

// check parses the text and checks it against its runners, as a2a-layer does when it starts
// (built-in tools a runner cannot give, for one). The config comes back when only that check
// fails.
func (d *Dashboard) check(text []byte) (*config.Config, error) {
	cfg, err := d.parse(text)
	if err != nil {
		return nil, err
	}
	runners, err := d.newRunners(cfg)
	if err != nil {
		return cfg, err
	}
	return cfg, server.Check(cfg, runners)
}

// change applies edit to the file and writes the result, unless it has problems the file did not
// have before. A file that is already broken can still be changed, as long as nothing new breaks.
func (d *Dashboard) change(edit func(text []byte) ([]byte, error)) error {
	d.edit.Lock()
	defer d.edit.Unlock()
	before, err := d.read()
	if err != nil {
		return err
	}
	after, err := edit(before)
	if err != nil {
		return err
	}
	_, errBefore := d.check(before)
	_, errAfter := d.check(after)
	if added := newProblems(errBefore, errAfter); len(added) > 0 {
		return &formError{added}
	}
	return d.write(after)
}

var lineRef = regexp.MustCompile(`line \d+: `)

// newProblems lists the problems in after that are not in before. Line numbers are ignored,
// since an edit moves the lines below it.
func newProblems(before, after error) []string {
	if after == nil {
		return nil
	}
	had := map[string]bool{}
	if before != nil {
		for _, l := range strings.Split(before.Error(), "\n") {
			had[lineRef.ReplaceAllString(strings.TrimSpace(l), "")] = true
		}
	}
	var out []string
	for _, l := range strings.Split(after.Error(), "\n") {
		if l = strings.TrimSpace(l); l != "" && !had[lineRef.ReplaceAllString(l, "")] {
			out = append(out, l)
		}
	}
	return out
}

// write replaces the file (the target of a symlink) with text, atomically.
func (d *Dashboard) write(text []byte) error {
	path := d.path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Page data.

// view is what every page gets; each page fills the fields it shows.
type view struct {
	Tab      string
	Path     string
	Problems []string // what is wrong with the file as it is
	Flash    string
	Error    []string // why the change just submitted was not made

	Agents   []agentRow
	Agent    agentForm
	CLIs     []string // the CLIs an agent can run on
	ForCLI   cliParts // the parts of the agent form that follow its CLI
	Settings settingsForm
	Play     playView
}

type agentRow struct {
	Name, Description, CLI, Model, URL string
	// SecretEnv is the environment variable the setup commands read the agent's secret from:
	// the one its secret refers to, or a suggested name ("" when it has no secret).
	SecretEnv        string
	MCP, Builtin     []string
	Remember, Secret bool
	Problems         []string
}

// cliParts are the parts of the agent form that follow its CLI: the built-in tools choice, and
// the models to pick from.
type cliParts struct {
	builtinForm
	// TakesRules says the CLI takes permission rules, like Examples.
	TakesRules bool
	Examples   []string
	// Models are those its CLI offers (ModelsNote says why there are none); DefaultModel is
	// what an empty model means.
	Models       []runner.Model
	ModelsNote   string
	DefaultModel string
}

// state is the file as a page shows it.
type state struct {
	text []byte
	cfg  *config.Config // nil when the file has problems
	err  error          // the problems
}

func (d *Dashboard) state() state {
	text, err := d.read()
	if err != nil {
		return state{err: err}
	}
	cfg, err := d.check(text)
	return state{text: text, cfg: cfg, err: err}
}

func (d *Dashboard) view(r *http.Request, tab string, st state) *view {
	v := &view{Tab: tab, Path: d.path}
	if st.err != nil {
		for _, l := range strings.Split(st.err.Error(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				v.Problems = append(v.Problems, l)
			}
		}
	}
	q := r.URL.Query()
	switch {
	case q.Get("saved") != "":
		v.Flash = "Saved " + q.Get("saved") + "."
	case q.Get("deleted") != "":
		v.Flash = "Deleted " + q.Get("deleted") + "."
	}
	return v
}

func (d *Dashboard) render(w http.ResponseWriter, page string, v *view) {
	var b bytes.Buffer
	if err := d.pages[page].ExecuteTemplate(&b, "layout", v); err != nil {
		d.log.Error("rendering a page", "page", page, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

// renderPart renders one template of parts.html, for htmx to swap into the page.
func (d *Dashboard) renderPart(w http.ResponseWriter, name string, data any) {
	var b bytes.Buffer
	if err := d.pages["playground"].ExecuteTemplate(&b, name, data); err != nil {
		d.log.Error("rendering a part", "part", name, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

func (d *Dashboard) part(name string, data func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { d.renderPart(w, name, data()) }
}

// done sends the browser on after a change: htmx follows HX-Location without a reload.
func done(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Location", fmt.Sprintf(`{"path":%q,"target":"body"}`, to))
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// problemsOf are the error lines about one entry ("agent pm: …").
func problemsOf(err error, prefix string) []string {
	if err == nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if strings.Contains(l, prefix) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// Agents.

func (d *Dashboard) agentsPage(w http.ResponseWriter, r *http.Request) {
	st := d.state()
	v := d.view(r, "agents", st)
	es, _ := entries(st.text, "agents")
	publicURL := ""
	if st.cfg != nil {
		publicURL = st.cfg.PublicURL
	}
	for _, e := range es {
		f, err := agentFromNode(e.Key, e.Value)
		row := agentRow{Name: e.Key, Description: f.Description, CLI: cmp.Or(f.CLI, config.DefaultCLI), Model: f.Model,
			Remember: f.Remember, Secret: f.Secret != "", Problems: problemsOf(st.err, "agent "+e.Key+":")}
		if err != nil {
			row.Problems = append(row.Problems, err.Error())
		}
		if publicURL != "" {
			row.URL = publicURL + "/" + e.Key
		}
		if f.Secret != "" {
			row.SecretEnv = secretEnv(e.Key, f.Secret)
		}
		for _, m := range f.MCP {
			row.MCP = append(row.MCP, m.Name)
		}
		row.Builtin = f.Builtin.tools()
		v.Agents = append(v.Agents, row)
	}
	d.render(w, "agents", v)
}

var envOnly = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// secretEnv names the environment variable holding an agent's secret: the one its secret is
// written as (${PM_SECRET}), or else <AGENT>_SECRET, for the caller to set.
func secretEnv(agent, raw string) string {
	if m := envOnly.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		return m[1]
	}
	return strings.ToUpper(strings.ReplaceAll(agent, "-", "_")) + "_SECRET"
}

func (d *Dashboard) agentPage(w http.ResponseWriter, r *http.Request) {
	st := d.state()
	v := d.view(r, "agents", st)
	if name := r.PathValue("name"); name != "" {
		n, err := entryValue(st.text, "agents", name)
		if err != nil || n == nil {
			http.NotFound(w, r)
			return
		}
		v.Agent, err = agentFromNode(name, n)
		if err != nil {
			v.Error = []string{err.Error()}
		}
	}
	d.renderAgentForm(w, v, st.text)
}

func (d *Dashboard) renderAgentForm(w http.ResponseWriter, v *view, text []byte) {
	v.CLIs = runner.Types()
	v.ForCLI = d.partsFor(text, v.Agent.CLI, v.Agent.Builtin)
	d.render(w, "agent", v)
}

// partsFor are the agent form's parts for a CLI ("" for the default one).
func (d *Dashboard) partsFor(text []byte, cli string, b builtinForm) cliParts {
	cli = cmp.Or(cli, config.DefaultCLI)
	if b.Mode == "" {
		b.Mode = "none"
	}
	info := runner.InfoFor(cli)
	p := cliParts{builtinForm: b, TakesRules: info.ToolRules, Examples: info.RuleExamples, DefaultModel: "the " + cli + " CLI's default"}
	p.Models, p.ModelsNote = d.models(cli, cliSettingsIn(text, cli).Command)
	return p
}

// modelsTTL is how long the models a CLI listed are remembered.
const modelsTTL = 5 * time.Minute

type listedModels struct {
	at     time.Time
	models []runner.Model
	note   string // why there are none
}

// models are what a runner's CLI offers, or a note saying why there are none.
func (d *Dashboard) models(typ, command string) ([]runner.Model, string) {
	key := typ + "\x00" + command
	d.mu.Lock()
	l, ok := d.modelCache[key]
	d.mu.Unlock()
	if ok && time.Since(l.at) < modelsTTL {
		return l.models, l.note
	}
	l = listedModels{at: time.Now()}
	r, err := runner.New(typ, runner.Options{Command: command})
	switch lister, ok := r.(runner.ModelLister); {
	case err != nil:
		l.note = err.Error()
	case !ok:
		l.note = "This CLI cannot list its models: type the name it takes"
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		l.models, err = lister.Models(ctx)
		cancel()
		if err != nil {
			l.note = "Could not list its models (" + err.Error() + ")"
		} else if len(l.models) == 0 {
			l.note = "It lists no models"
		}
	}
	d.mu.Lock()
	d.modelCache[key] = l
	d.mu.Unlock()
	return l.models, l.note
}

// cliPart re-renders the agent form's parts for its CLI when the CLI changes, keeping what
// was chosen.
func (d *Dashboard) cliPart(w http.ResponseWriter, r *http.Request) {
	text, err := d.read()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	d.renderPart(w, "cli-parts", d.partsFor(text, q.Get("cli"), builtinFromForm(q)))
}

func (d *Dashboard) saveAgent(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f, err := agentFromForm(r.PostForm)
	if err == nil {
		err = d.change(func(text []byte) ([]byte, error) {
			if f.Name != f.Original {
				if n, _ := entryValue(text, "agents", f.Name); n != nil {
					return nil, fmt.Errorf("there is already an agent named %s", f.Name)
				}
			}
			old, err := entryValue(text, "agents", f.Original)
			if err != nil {
				return nil, err
			}
			return putEntry(text, "agents", f.Original, f.Name, f.node(old))
		})
	}
	if err != nil {
		st := d.state()
		v := d.view(r, "agents", st)
		v.Agent, v.Error = f, errorLines(err)
		d.renderAgentForm(w, v, st.text)
		return
	}
	d.log.Info("agent saved", "agent", f.Name)
	done(w, r, "/agents?saved="+f.Name)
}

func (d *Dashboard) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := d.change(func(text []byte) ([]byte, error) { return removeEntry(text, "agents", name) })
	if err != nil {
		v := d.view(r, "agents", d.state())
		v.Error = errorLines(err)
		d.render(w, "agents", v)
		return
	}
	d.log.Info("agent deleted", "agent", name)
	done(w, r, "/agents?deleted="+name)
}

// agentCard shows the agent's A2A card as the playground serves it.
func (d *Dashboard) agentCard(w http.ResponseWriter, r *http.Request) {
	pg, err := d.playground()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + r.PathValue("name") + "/.well-known/agent-card.json"
	pg.srv.Handler().ServeHTTP(w, r2)
}

// Settings.

func (d *Dashboard) settingsPage(w http.ResponseWriter, r *http.Request) {
	st := d.state()
	v := d.view(r, "settings", st)
	v.Settings, _ = settingsFromText(st.text)
	d.render(w, "settings", v)
}

func (d *Dashboard) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s, err := settingsFromForm(r.PostForm)
	if err == nil {
		err = d.change(func(text []byte) ([]byte, error) { return putSettings(text, s) })
	}
	if err != nil {
		v := d.view(r, "settings", d.state())
		v.Settings, v.Error = s, errorLines(err)
		d.render(w, "settings", v)
		return
	}
	d.log.Info("settings saved")
	done(w, r, "/settings?saved=the+settings")
}

func errorLines(err error) []string {
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Playground.

type playView struct {
	Agents        []playAgent
	Agent         string // the conversation's agent, or the one picked for a new conversation
	Selected      *playAgent
	Unready       string     // why nothing can run
	Conversations []convView // most recently used first
	Polling       bool       // a run is unfinished: the list refreshes itself
	Conv          *convView  // the conversation shown; nil for a new one
	Runs          []runView  // its runs, oldest first
	Gone          bool       // the conversation asked for is no longer kept
	Blocked       string     // why the conversation shown cannot continue
	Stale         bool       // the config changed since its last run
	Headers       string     // the request headers, as last sent
}

// convList is the playground's list of conversations, conv the one shown ("" for none).
type convList struct {
	Conversations []convView
	Polling       bool
	Conv          string
	OOB           bool // swapped in out of band, beside a run
}

func (p playView) List() convList {
	l := convList{Conversations: p.Conversations, Polling: p.Polling}
	if p.Conv != nil {
		l.Conv = p.Conv.ID
	}
	return l
}

// playAgent is an agent as the playground describes it.
type playAgent struct {
	Name, Description, CLI, Model, URL, Timeout string
	MaxTurns                                    int
	Remember, Secret                            bool
	MCP                                         []playMCP
	Builtin                                     []string
	Examples                                    []string
}

type playMCP struct {
	Name, URL string
	Tools     []string
}

func describe(cfg *config.Config, name string) *playAgent {
	a := cfg.Agents[name]
	if a == nil {
		return nil
	}
	p := &playAgent{Name: name, Description: a.Description, CLI: a.CLI, Model: a.Model, URL: cfg.PublicURL + "/" + name,
		Timeout: shortDuration(time.Duration(a.Timeout)), MaxTurns: a.MaxTurns, Remember: a.Context.Remember, Secret: a.Secret != "",
		Builtin: a.BuiltinTools}
	for _, s := range a.Skills {
		p.Examples = append(p.Examples, s.Examples...)
	}
	for _, n := range slices.Sorted(maps.Keys(a.MCP)) {
		p.MCP = append(p.MCP, playMCP{Name: n, URL: a.MCP[n].URL, Tools: a.MCP[n].Tools})
	}
	return p
}

// shortDuration writes 15m rather than 15m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func (d *Dashboard) playgroundPage(w http.ResponseWriter, r *http.Request) {
	v := d.view(r, "playground", d.state())
	v.Play = d.playView(r.URL.Query().Get("c"), r.URL.Query().Get("agent"))
	d.render(w, "playground", v)
}

// playView describes the playground showing conversation convID, or a new conversation with
// agent (the first one by default).
func (d *Dashboard) playView(convID, agent string) playView {
	var v playView
	pg, err := d.playground()
	d.mu.Lock()
	current := d.play
	var conv *conversation
	for _, c := range d.convs {
		cv := c.view()
		if c.ID == convID {
			conv, cv.On = c, true
			v.Conv = &cv
			for _, rn := range c.runs {
				v.Runs = append(v.Runs, rn.view(current))
			}
			v.Stale = len(c.runs) > 0 && c.runs[len(c.runs)-1].pg != current
		}
		v.Polling = v.Polling || cv.Running
		v.Conversations = append(v.Conversations, cv)
	}
	d.mu.Unlock()
	v.Gone = convID != "" && conv == nil
	if conv != nil {
		agent = conv.Agent
	}
	if err != nil {
		v.Unready = err.Error()
		return v
	}
	for _, name := range pg.cfg.AgentNames() {
		v.Agents = append(v.Agents, *describe(pg.cfg, name))
	}
	if pg.cfg.Agents[agent] == nil && conv == nil {
		agent = pg.cfg.AgentNames()[0]
	}
	v.Agent, v.Selected = agent, describe(pg.cfg, agent)
	if conv != nil {
		switch a := pg.cfg.Agents[agent]; {
		case a == nil:
			v.Blocked = agent + " is no longer in the config, so this conversation cannot continue."
		case !a.Context.Remember:
			v.Blocked = agent + " does not remember earlier tasks, so this conversation cannot continue."
		}
	}
	return v
}

// conversationList refreshes the playground's list of conversations while runs are going.
func (d *Dashboard) conversationList(w http.ResponseWriter, r *http.Request) {
	d.renderPart(w, "conv-list", d.playView(r.URL.Query().Get("c"), "").List())
}

// playgroundAgent describes the agent picked in the playground's form.
func (d *Dashboard) playgroundAgent(w http.ResponseWriter, r *http.Request) {
	pg, err := d.playground()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	d.renderPart(w, "agent-info", describe(pg.cfg, r.URL.Query().Get("agent")))
}

func (d *Dashboard) newRun(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	convID := r.PostForm.Get("conversation")
	agent := r.PostForm.Get("agent")
	prompt := strings.TrimSpace(strings.ReplaceAll(r.PostForm.Get("message"), "\r\n", "\n"))
	contextID := strings.TrimSpace(r.PostForm.Get("context"))
	if prompt == "" {
		d.renderPart(w, "run-error", "Write a message for the agent first.")
		return
	}
	headers := r.PostForm.Get("headers")
	keys, values, err := parsePairs(block(headers), ":")
	if err != nil {
		d.renderPart(w, "run-error", "Headers: "+err.Error())
		return
	}
	header := http.Header{}
	for _, k := range keys {
		header.Set(k, values[k])
	}
	conv, rn, err := d.startRun(convID, agent, contextID, prompt, header)
	if err != nil {
		d.renderPart(w, "run-error", err.Error())
		return
	}
	d.log.Info("playground run", "agent", rn.Agent, "task", rn.ID)
	w.Header().Set("HX-Trigger", "run-sent")
	v := d.playView(conv.ID, "")
	if conv.ID != convID {
		// A new conversation: show it in place of the empty one.
		v.Headers = headers
		w.Header().Set("HX-Retarget", "#chat")
		w.Header().Set("HX-Reswap", "outerHTML")
		w.Header().Set("HX-Push-Url", "/playground?c="+conv.ID)
		d.renderPart(w, "chat", v)
		return
	}
	d.mu.Lock()
	current := d.play
	d.mu.Unlock()
	list := v.List()
	list.OOB = true
	d.renderPart(w, "sent", struct {
		Run  runView
		List convList
	}{rn.view(current), list})
}

func (d *Dashboard) renderRun(w http.ResponseWriter, rn *run) {
	d.mu.Lock()
	current := d.play
	d.mu.Unlock()
	d.renderPart(w, "run", rn.view(current))
}

func (d *Dashboard) showRun(w http.ResponseWriter, r *http.Request) {
	rn := d.findRun(r.PathValue("id"))
	if rn == nil {
		// Gone from the list (or the dashboard restarted): stop the page polling it.
		w.WriteHeader(http.StatusOK)
		return
	}
	d.renderRun(w, rn)
}

func (d *Dashboard) cancelRunHandler(w http.ResponseWriter, r *http.Request) {
	rn := d.findRun(r.PathValue("id"))
	if rn == nil {
		http.NotFound(w, r)
		return
	}
	if err := d.cancelRun(rn); err != nil {
		d.log.Warn("could not cancel a playground run", "task", rn.ID, "err", err)
	}
	d.renderRun(w, rn)
}
