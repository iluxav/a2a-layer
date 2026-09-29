package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/server"
)

// The playground runs the agents as the config file defines them now, on a server.Server of its
// own: the same code path as a2a-layer, reached in process over A2A (message/send, tasks/get,
// tasks/cancel). Its tasks' MCP proxy is served on the dashboard's listener.
//
// When the file changes, the next run gets a new server; the old one keeps its running tasks and
// is shut down once they finish. A conversation (an agent that remembers) therefore continues
// only while the config is unchanged.

// maxRuns is how many runs the playground lists.
const maxRuns = 30

// playground is a server built from one version of the config.
type playground struct {
	hash    [32]byte
	cfg     *config.Config
	srv     *server.Server
	active  int  // runs not finished yet; guarded by Dashboard.mu
	retired bool // a newer config replaced it; guarded by Dashboard.mu
}

// run is one message sent from the playground, followed until its task finishes.
type run struct {
	ID, Agent, ContextID, Prompt string
	Started                      time.Time
	Remember                     bool   // the agent keeps the conversation, so it can be continued
	Note                         string // said about the run when it started
	pg                           *playground

	mu    sync.Mutex
	task  a2a.Task
	notes []string // the progress the task reported, oldest first
	lastM string   // the id of the status message last added to notes
	done  bool
	ended time.Time
}

// runView is a run as the page shows it.
type runView struct {
	ID, Agent, ContextID, Prompt, Note string
	State, Answer, Failure             string
	Notes                              []string
	Done, Remember, Stale              bool
	Elapsed                            string
	Meta                               []metaItem
}

type metaItem struct{ Label, Value string }

func (r *run) view(current *playground) runView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := runView{ID: r.ID, Agent: r.Agent, ContextID: r.ContextID, Prompt: r.Prompt, Note: r.Note,
		State: r.task.Status.State, Notes: r.notes, Done: r.done, Remember: r.Remember, Stale: r.pg != current}
	end := time.Now()
	if r.done {
		end = r.ended
	}
	v.Elapsed = end.Sub(r.Started).Round(time.Second).String()
	for _, a := range r.task.Artifacts {
		for _, p := range a.Parts {
			v.Answer += p.Text
		}
	}
	// The agent's last words are usually its answer too: don't show them twice.
	if n := len(v.Notes); n > 0 && v.Answer != "" {
		last, answer := v.Notes[n-1], strings.TrimSpace(v.Answer)
		if last == answer || strings.HasSuffix(last, "…") && strings.HasPrefix(answer, strings.TrimSuffix(last, "…")) {
			v.Notes = v.Notes[:n-1]
		}
	}
	if r.task.Status.State == a2a.StateFailed || r.task.Status.State == a2a.StateCanceled {
		if m := r.task.Status.Message; m != nil {
			v.Failure = m.Text()
		}
		if len(v.Notes) > 0 && v.Notes[len(v.Notes)-1] == v.Failure {
			v.Notes = v.Notes[:len(v.Notes)-1]
		}
	}
	md := r.task.Metadata
	add := func(label, key, format string) {
		if x, ok := md[key]; ok {
			v.Meta = append(v.Meta, metaItem{label, fmt.Sprintf(format, x)})
		}
	}
	add("model", "model", "%v")
	add("turns", "turns", "%v")
	add("cost", "cost_usd", "$%.4f")
	add("tokens in", "input_tokens", "%v")
	add("tokens out", "output_tokens", "%v")
	add("conversation task", "context_task", "%v")
	return v
}

// playground returns the playground for the config file as it is now, building a new one when
// the file changed since the last.
func (d *Dashboard) playground() (*playground, error) {
	text, err := d.read()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(text)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.play != nil && d.play.hash == sum {
		return d.play, nil
	}
	cfg, err := d.parse(text)
	if err != nil {
		return nil, err
	}
	if len(cfg.Agents) == 0 {
		return nil, errors.New("there are no agents yet")
	}
	runners, err := d.newRunners(cfg)
	if err != nil {
		return nil, err
	}
	srv, err := server.New(cfg, runners, d.log, server.WithProxy(d.proxy))
	if err != nil {
		return nil, err
	}
	if old := d.play; old != nil {
		old.retired = true
		d.releaseLocked(old)
	}
	d.play = &playground{hash: sum, cfg: cfg, srv: srv}
	d.live = append(d.live, d.play)
	return d.play, nil
}

// call makes one A2A JSON-RPC call to an agent of the playground, in process.
func (p *playground) call(agent, method string, params any, header http.Header) (json.RawMessage, error) {
	a := p.cfg.Agents[agent]
	if a == nil {
		return nil, fmt.Errorf("no agent %q", agent)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req := httptest.NewRequest(http.MethodPost, "/"+agent, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+a.Secret)
	}
	rec := httptest.NewRecorder()
	p.srv.Handler().ServeHTTP(rec, req)
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *a2a.Error      `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("%s: HTTP %d: %s", method, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// startRun sends a message to an agent and follows its task in the background.
func (d *Dashboard) startRun(agent, contextID, prompt string, header http.Header) (*run, error) {
	pg, err := d.playground()
	if err != nil {
		return nil, err
	}
	a := pg.cfg.Agents[agent]
	if a == nil {
		return nil, fmt.Errorf("no agent %q in the config", agent)
	}
	var note string
	if contextID != "" && a.Context.Remember {
		if prev := d.findContext(agent, contextID); prev != nil && prev.pg != pg {
			note = "The config changed since this conversation's last run, so this starts a new one."
		}
	}
	d.mu.Lock()
	pg.active++
	d.mu.Unlock()
	msg := a2a.Message{Kind: "message", Role: "user", MessageID: newRow() + newRow(), ContextID: contextID, Parts: []a2a.Part{a2a.TextPart(prompt)}}
	blocking := false
	raw, err := pg.call(agent, "message/send", a2a.SendParams{Message: msg, Configuration: &a2a.SendConfig{Blocking: &blocking}}, header)
	if err != nil {
		d.finished(pg)
		return nil, err
	}
	var t a2a.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		d.finished(pg)
		return nil, err
	}
	r := &run{ID: t.ID, Agent: agent, ContextID: t.ContextID, Prompt: prompt, Started: time.Now(), Remember: a.Context.Remember, Note: note, pg: pg, task: t}
	d.mu.Lock()
	d.runs = append([]*run{r}, d.runs...)
	if len(d.runs) > maxRuns {
		d.runs = d.runs[:maxRuns]
	}
	d.mu.Unlock()
	go d.follow(r)
	return r, nil
}

// follow polls a run's task until it finishes, keeping each progress note it reports.
func (d *Dashboard) follow(r *run) {
	defer d.finished(r.pg)
	for {
		raw, err := r.pg.call(r.Agent, "tasks/get", a2a.TaskParams{ID: r.ID}, nil)
		var t a2a.Task
		if err == nil {
			err = json.Unmarshal(raw, &t)
		}
		r.mu.Lock()
		if err != nil {
			r.task.Status = a2a.TaskStatus{State: a2a.StateFailed, Message: &a2a.Message{Parts: []a2a.Part{a2a.TextPart("lost the task: " + err.Error())}}}
			r.done, r.ended = true, time.Now()
			r.mu.Unlock()
			return
		}
		r.task = t
		if m := t.Status.Message; m != nil && m.MessageID != r.lastM {
			r.lastM = m.MessageID
			r.notes = append(r.notes, m.Text())
		}
		if a2a.Terminal(t.Status.State) {
			r.done, r.ended = true, time.Now()
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		time.Sleep(250 * time.Millisecond)
	}
}

// finished releases a run's hold on its playground.
func (d *Dashboard) finished(pg *playground) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pg.active--
	d.releaseLocked(pg)
}

// releaseLocked shuts pg down once a newer config replaced it and its last run finished.
// Call with d.mu held.
func (d *Dashboard) releaseLocked(pg *playground) {
	if pg.retired && pg.active == 0 {
		d.live = slices.DeleteFunc(d.live, func(p *playground) bool { return p == pg })
		go pg.srv.Shutdown()
	}
}

func (d *Dashboard) cancelRun(r *run) error {
	_, err := r.pg.call(r.Agent, "tasks/cancel", a2a.TaskParams{ID: r.ID}, nil)
	return err
}

func (d *Dashboard) findRun(id string) *run {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (d *Dashboard) findContext(agent, contextID string) *run {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.runs {
		if r.Agent == agent && r.ContextID == contextID {
			return r
		}
	}
	return nil
}

// Shutdown stops every playground server, canceling the runs still going.
func (d *Dashboard) Shutdown() {
	d.mu.Lock()
	live := d.live
	d.live = nil
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, pg := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pg.srv.Shutdown()
		}()
	}
	wg.Wait()
}
