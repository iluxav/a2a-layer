package server

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/iluxav/a2a-layer/internal/a2a"
	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/runner"
)

// sessionRunner stands in for a CLI with sessions: a new job gets a new session id, a resumed
// one keeps its id. A job whose prompt is "hold" waits for release.
type sessionRunner struct {
	mu        sync.Mutex
	jobs      []runner.Job
	next      int
	gone      map[string]bool // sessions to report as missing
	forgotten []string
	release   chan struct{}
	started   chan string
}

func (f *sessionRunner) Run(ctx context.Context, job runner.Job, _ runner.Progress) (runner.Result, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	if job.Resume != "" && f.gone[job.Resume] {
		f.mu.Unlock()
		return runner.Result{}, runner.ErrSessionNotFound
	}
	id := job.Resume
	if id == "" {
		f.next++
		id = fmt.Sprintf("session-%d", f.next)
	}
	f.mu.Unlock()
	if f.started != nil {
		f.started <- job.Prompt
	}
	if job.Prompt == "hold" {
		select {
		case <-f.release:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
	return runner.Result{Text: "ok: " + job.Prompt, SessionID: id}, nil
}

func (f *sessionRunner) ForgetSessions(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, dir)
	return nil
}

func (f *sessionRunner) job(i int) runner.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[i]
}

func newConvServer(t *testing.T, f *sessionRunner, maxTasks, maxParallel int) (*Server, *httptest.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := func(name string, remember bool) *config.Agent {
		return &config.Agent{
			Name: name, Description: "Remembers.", Version: "1", CLI: "claude", MaxTurns: 3,
			Timeout: config.Duration(time.Minute), MaxParallel: maxParallel,
			Skills:  []config.Skill{{ID: name, Name: name, Description: name}},
			Context: config.Context{Remember: remember, IdleTimeout: config.Duration(time.Hour), MaxTasks: maxTasks},
		}
	}
	cfg := &config.Config{
		Listen: ln.Addr().String(), PublicURL: "http://agents.local", WorkDir: t.TempDir(),
		Agents: map[string]*config.Agent{"mem": agent("mem", true), "plain": agent("plain", false)},
	}
	s, err := New(cfg, map[string]runner.Runner{"claude": f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return s, ts
}

func sendIn(t *testing.T, ts *httptest.Server, agent, contextID, text string, blocking bool) a2a.Task {
	t.Helper()
	p := sendParams(text, &blocking)
	if contextID != "" {
		p["message"].(map[string]any)["contextId"] = contextID
	}
	return asTask(t, call(t, ts.URL+"/"+agent, "", "message/send", p, nil))
}

func stateOf(t *testing.T, ts *httptest.Server, agent, id string) string {
	t.Helper()
	return asTask(t, call(t, ts.URL+"/"+agent, "", "tasks/get", map[string]any{"id": id}, nil)).Status.State
}

// Tasks of one context continue one session in one directory; another context gets its own.
// An agent that does not remember keeps nothing.
func TestAContextsTasksContinueOneSession(t *testing.T) {
	f := &sessionRunner{}
	_, ts := newConvServer(t, f, 20, 1)
	first := sendIn(t, ts, "mem", "run-a", "build the page", true)
	second := sendIn(t, ts, "mem", "run-a", "fix the button", true)
	other := sendIn(t, ts, "mem", "run-b", "unrelated", true)
	plain := sendIn(t, ts, "plain", "run-a", "stateless", true)
	for _, task := range []a2a.Task{first, second, other, plain} {
		if task.Status.State != a2a.StateCompleted {
			t.Fatalf("task %s: %s", task.ID, task.Status.State)
		}
	}
	j1, j2, j3, j4 := f.job(0), f.job(1), f.job(2), f.job(3)
	if j1.Resume != "" || !j1.KeepSession {
		t.Errorf("first task: resume %q keep %v", j1.Resume, j1.KeepSession)
	}
	if j2.Resume != "session-1" || j2.WorkDir != j1.WorkDir {
		t.Errorf("second task did not continue the first: resume %q, dir %q vs %q", j2.Resume, j2.WorkDir, j1.WorkDir)
	}
	if j3.Resume != "" || j3.WorkDir == j1.WorkDir {
		t.Errorf("another context shared the first one's session or dir: %+v", j3)
	}
	if j4.Resume != "" || j4.KeepSession {
		t.Errorf("a stateless agent kept or resumed a session: %+v", j4)
	}
	if second.Metadata["resumed"] != true || second.Metadata["context_task"] != float64(2) {
		t.Errorf("second task metadata = %v", second.Metadata)
	}
}

// A context's tasks take turns; other contexts are not held up by it.
func TestAContextsTasksTakeTurns(t *testing.T) {
	f := &sessionRunner{release: make(chan struct{}), started: make(chan string, 4)}
	_, ts := newConvServer(t, f, 20, 3)
	a := sendIn(t, ts, "mem", "run-a", "hold", false)
	if got := <-f.started; got != "hold" {
		t.Fatalf("started %q", got)
	}
	b := sendIn(t, ts, "mem", "run-a", "next in a", false)
	c := sendIn(t, ts, "mem", "run-b", "in b", false)
	select {
	case got := <-f.started:
		if got != "in b" {
			t.Fatalf("%q started while its context was busy", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("another context was held up")
	}
	time.Sleep(50 * time.Millisecond)
	if st := stateOf(t, ts, "mem", b.ID); st != a2a.StateSubmitted {
		t.Errorf("the second task of a busy context is %s, want submitted", st)
	}
	close(f.release)
	select {
	case got := <-f.started:
		if got != "next in a" {
			t.Fatalf("started %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting task never ran")
	}
	_ = a
	_ = c
}

// Past max_tasks the session starts afresh; a session that went missing is replaced.
func TestSessionRestartsAfterMaxTasksAndWhenMissing(t *testing.T) {
	f := &sessionRunner{gone: map[string]bool{}}
	_, ts := newConvServer(t, f, 2, 1)
	sendIn(t, ts, "mem", "run-a", "one", true)
	sendIn(t, ts, "mem", "run-a", "two", true)
	third := sendIn(t, ts, "mem", "run-a", "three", true)
	if f.job(2).Resume != "" || third.Status.State != a2a.StateCompleted {
		t.Errorf("third task resumed %q past max_tasks 2", f.job(2).Resume)
	}
	f.mu.Lock()
	f.gone["session-2"] = true
	f.mu.Unlock()
	fourth := sendIn(t, ts, "mem", "run-a", "four", true)
	if fourth.Status.State != a2a.StateCompleted || f.job(3).Resume != "session-2" || f.job(4).Resume != "" {
		t.Errorf("missing session: state %s, jobs %+v %+v", fourth.Status.State, f.job(3), f.job(4))
	}
}

// An idle conversation ends: the runner forgets its sessions and its directory is deleted.
// A later task of the same context starts over.
func TestIdleConversationsEnd(t *testing.T) {
	f := &sessionRunner{}
	s, ts := newConvServer(t, f, 20, 1)
	sendIn(t, ts, "mem", "run-a", "one", true)
	dir := f.job(0).WorkDir
	if ended := s.convs.sweep(time.Now(), false); len(ended) != 0 {
		t.Fatal("a fresh conversation ended")
	}
	s.endConversations(s.convs.sweep(time.Now().Add(2*time.Hour), false))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the conversation's directory is still there: %v", err)
	}
	f.mu.Lock()
	forgot := len(f.forgotten) == 1 && f.forgotten[0] == dir
	f.mu.Unlock()
	if !forgot {
		t.Errorf("forgotten = %v", f.forgotten)
	}
	sendIn(t, ts, "mem", "run-a", "two", true)
	if j := f.job(1); j.Resume != "" || j.WorkDir == dir {
		t.Errorf("an ended conversation was continued: %+v", j)
	}
}
