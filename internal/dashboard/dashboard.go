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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

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

	mu   sync.Mutex
	play *playground   // built from the file as it was last read
	live []*playground // play, and replaced ones whose runs have not finished
	runs []*run        // newest first
}

// New builds the dashboard.
func New(o Options) (*Dashboard, error) {
	d := &Dashboard{path: o.Path, anyHost: o.AnyHost, log: o.Log, proxy: mcpproxy.New(o.BaseURL), newRunners: o.NewRunners, pages: map[string]*template.Template{}}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.newRunners == nil {
		d.newRunners = server.NewRunners
	}
	for _, page := range []string{"agents", "agent", "runners", "runner", "settings", "playground"} {
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
	mux.HandleFunc("GET /parts/skill", d.part("skill-row", func() any { return skillForm{Row: newRow()} }))

	mux.HandleFunc("GET /runners", d.runnersPage)
	mux.HandleFunc("GET /runners/new", d.runnerPage)
	mux.HandleFunc("GET /runners/{name}/edit", d.runnerPage)
	mux.HandleFunc("POST /runners", d.saveRunner)
	mux.HandleFunc("POST /runners/{name}/delete", d.deleteRunner)

	mux.HandleFunc("GET /settings", d.settingsPage)
	mux.HandleFunc("POST /settings", d.saveSettings)

	mux.HandleFunc("GET /playground", d.playgroundPage)
	mux.HandleFunc("GET /playground/agent", d.playgroundAgent)
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
	_, errBefore := d.parse(before)
	_, errAfter := d.parse(after)
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

	Agents  []agentRow
	Agent   agentForm
	Runners []runnerRow
	Runner  runnerForm
	// RunnerNames are the runners an agent can pick; DefaultRunner is the one it gets without
	// picking (empty when there are several).
	RunnerNames   []string
	DefaultRunner string
	RunnerTypes   []string
	Settings      settingsForm
	Play          playView
}

type agentRow struct {
	Name, Description, Runner, Model, URL string
	MCP, Builtin                          []string
	Remember, Secret                      bool
	Problems                              []string
}

type runnerRow struct {
	Name, Type, Bin, Model, MaxTurns, Timeout string
	Found, Implicit                           bool
	UsedBy                                    []string
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
	cfg, err := d.parse(text)
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

// runnerChoices are the runners in the file, or the implicit default when there are none.
func runnerChoices(text []byte) []string {
	es, _ := entries(text, "runners")
	if len(es) == 0 {
		return []string{config.DefaultRunner}
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Key)
	}
	return out
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
		row := agentRow{Name: e.Key, Description: f.Description, Runner: f.Runner, Model: f.Model,
			Remember: f.Remember, Secret: f.Secret != "", Problems: problemsOf(st.err, "agent "+e.Key+":")}
		if err != nil {
			row.Problems = append(row.Problems, err.Error())
		}
		if st.cfg != nil {
			if a := st.cfg.Agents[e.Key]; a != nil {
				row.Runner, row.Model = a.Runner, a.Model
			}
		}
		if publicURL != "" {
			row.URL = publicURL + "/" + e.Key
		}
		for _, m := range f.MCP {
			row.MCP = append(row.MCP, m.Name)
		}
		row.Builtin = lines(f.BuiltinTools)
		v.Agents = append(v.Agents, row)
	}
	d.render(w, "agents", v)
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
	v.RunnerNames = runnerChoices(text)
	if len(v.RunnerNames) == 1 {
		v.DefaultRunner = v.RunnerNames[0]
	}
	d.render(w, "agent", v)
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

// Runners.

func (d *Dashboard) runnersPage(w http.ResponseWriter, r *http.Request) {
	st := d.state()
	v := d.view(r, "runners", st)
	es, _ := entries(st.text, "runners")
	if len(es) == 0 {
		es = []entry{{Key: config.DefaultRunner}}
	}
	for _, e := range es {
		f, _ := runnerFromNode(e.Key, e.Value)
		rc := config.Runner{Type: f.Type, Command: f.Command}
		if rc.Type == "" {
			rc.Type = e.Key
		}
		row := runnerRow{Name: e.Key, Type: rc.Type, Bin: rc.Bin(), Model: f.Model, MaxTurns: f.MaxTurns, Timeout: f.Timeout, Implicit: e.Value == nil}
		_, err := exec.LookPath(row.Bin)
		row.Found = err == nil
		if st.cfg != nil {
			for _, name := range st.cfg.AgentNames() {
				if st.cfg.Agents[name].Runner == e.Key {
					row.UsedBy = append(row.UsedBy, name)
				}
			}
		}
		v.Runners = append(v.Runners, row)
	}
	d.render(w, "runners", v)
}

func (d *Dashboard) runnerPage(w http.ResponseWriter, r *http.Request) {
	st := d.state()
	v := d.view(r, "runners", st)
	v.RunnerTypes = runner.Types()
	v.Runner = runnerForm{Type: config.DefaultRunner}
	if name := r.PathValue("name"); name != "" {
		n, err := entryValue(st.text, "runners", name)
		if err != nil || (n == nil && name != config.DefaultRunner) {
			http.NotFound(w, r)
			return
		}
		v.Runner, err = runnerFromNode(name, n)
		if err != nil {
			v.Error = []string{err.Error()}
		}
		if n == nil {
			v.Runner.Original = "" // the implicit default: saving writes it
		}
		if v.Runner.Type == "" {
			v.Runner.Type = name
		}
	}
	d.render(w, "runner", v)
}

func (d *Dashboard) saveRunner(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f, err := runnerFromForm(r.PostForm)
	if err == nil {
		err = d.change(func(text []byte) ([]byte, error) { return putRunner(text, f) })
	}
	if err != nil {
		v := d.view(r, "runners", d.state())
		v.RunnerTypes = runner.Types()
		v.Runner, v.Error = f, errorLines(err)
		d.render(w, "runner", v)
		return
	}
	d.log.Info("runner saved", "runner", f.Name)
	done(w, r, "/runners?saved="+f.Name)
}

// putRunner writes a runner, keeping every agent on the runner it had: agents that name a
// renamed runner follow it, and agents that relied on its being the only one name it once
// there are several.
func putRunner(text []byte, f runnerForm) ([]byte, error) {
	if f.Name != f.Original {
		if n, _ := entryValue(text, "runners", f.Name); n != nil {
			return nil, fmt.Errorf("there is already a runner named %s", f.Name)
		}
	}
	before := runnerChoices(text)
	var err error
	if es, _ := entries(text, "runners"); len(es) == 0 && f.Name != config.DefaultRunner {
		// Without runners the agents run on the implicit default; keep it beside the new one.
		if text, err = putEntry(text, "runners", "", config.DefaultRunner, &yaml.Node{Kind: yaml.MappingNode}); err != nil {
			return nil, err
		}
	}
	old, err := entryValue(text, "runners", f.Original)
	if err != nil {
		return nil, err
	}
	if text, err = putEntry(text, "runners", f.Original, f.Name, f.node(old)); err != nil {
		return nil, err
	}
	after := runnerChoices(text)
	agents, err := entries(text, "agents")
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		var raw struct {
			Runner string `yaml:"runner"`
		}
		a.Value.Decode(&raw)
		want := raw.Runner
		switch {
		case raw.Runner != "" && raw.Runner == f.Original:
			want = f.Name
		case raw.Runner == "" && len(before) == 1 && len(after) > 1:
			want = before[0]
			if want == f.Original {
				want = f.Name
			}
		}
		if want == raw.Runner {
			continue
		}
		setScalar(a.Value, "runner", want, strNode)
		if text, err = putEntry(text, "agents", a.Key, a.Key, a.Value); err != nil {
			return nil, err
		}
	}
	return text, nil
}

func (d *Dashboard) deleteRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := d.change(func(text []byte) ([]byte, error) { return removeEntry(text, "runners", name) })
	if err != nil {
		st := d.state()
		v := d.view(r, "runners", st)
		v.Error = errorLines(err)
		d.render(w, "runners", v)
		return
	}
	d.log.Info("runner deleted", "runner", name)
	done(w, r, "/runners?deleted="+name)
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
	s := settingsFromForm(r.PostForm)
	err := d.change(func(text []byte) ([]byte, error) {
		have, err := settingsFromText(text)
		if err != nil {
			return nil, err
		}
		old := have.values()
		for i, kv := range s.values() {
			key, val := kv[0], kv[1]
			switch {
			case val == block(old[i][1]):
				continue // unchanged: keep it as written
			case val == "":
				text, err = removeEntry(text, "", key)
			default:
				text, err = putEntry(text, "", key, key, strNode(val))
			}
			if err != nil {
				return nil, err
			}
		}
		return text, nil
	})
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
	Agents    []playAgent
	Agent     string // the one selected
	Selected  *playAgent
	Runs      []runView
	Unready   string // why nothing can run
	ContextID string
}

// playAgent is an agent as the playground describes it.
type playAgent struct {
	Name, Description, Runner, Model, URL, Timeout string
	MaxTurns                                       int
	Remember, Secret                               bool
	MCP                                            []playMCP
	Builtin                                        []string
	Examples                                       []string
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
	p := &playAgent{Name: name, Description: a.Description, Runner: a.Runner, Model: a.Model, URL: cfg.PublicURL + "/" + name,
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
	st := d.state()
	v := d.view(r, "playground", st)
	pg, err := d.playground()
	if err != nil {
		v.Play.Unready = err.Error()
	} else {
		for _, name := range pg.cfg.AgentNames() {
			v.Play.Agents = append(v.Play.Agents, *describe(pg.cfg, name))
		}
		v.Play.Agent = r.URL.Query().Get("agent")
		if pg.cfg.Agents[v.Play.Agent] == nil {
			v.Play.Agent = pg.cfg.AgentNames()[0]
		}
		v.Play.Selected = describe(pg.cfg, v.Play.Agent)
	}
	d.mu.Lock()
	runs := slices.Clone(d.runs)
	current := d.play
	d.mu.Unlock()
	for _, rn := range runs {
		v.Play.Runs = append(v.Play.Runs, rn.view(current))
	}
	d.render(w, "playground", v)
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
	agent := r.PostForm.Get("agent")
	prompt := strings.TrimSpace(strings.ReplaceAll(r.PostForm.Get("message"), "\r\n", "\n"))
	contextID := strings.TrimSpace(r.PostForm.Get("context"))
	if prompt == "" {
		d.renderPart(w, "run-error", "Write a message for the agent first.")
		return
	}
	keys, values, err := parsePairs(block(r.PostForm.Get("headers")), ":")
	if err != nil {
		d.renderPart(w, "run-error", "Headers: "+err.Error())
		return
	}
	header := http.Header{}
	for _, k := range keys {
		header.Set(k, values[k])
	}
	rn, err := d.startRun(agent, contextID, prompt, header)
	if err != nil {
		d.renderPart(w, "run-error", err.Error())
		return
	}
	d.log.Info("playground run", "agent", agent, "task", rn.ID)
	d.renderRun(w, rn)
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
