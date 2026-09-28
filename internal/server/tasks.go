package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/iluxav/a2a-layer/internal/a2a"
)

// finishedTTL is how long a finished task stays readable with tasks/get.
const finishedTTL = time.Hour

// task is one message/send's unit of work.
type task struct {
	id, contextID, agent string
	request              a2a.Message

	mu        sync.Mutex
	state     string
	status    *a2a.Message // the agent's latest word
	artifacts []a2a.Artifact
	metadata  map[string]any
	updated   time.Time
	finished  time.Time

	cancel context.CancelFunc
	done   chan struct{} // closed when the task reaches a terminal state
}

// snapshot is the task as the wire shows it.
func (t *task) snapshot(historyLength *int) a2a.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := a2a.Task{
		Kind: "task", ID: t.id, ContextID: t.contextID,
		Status:    a2a.TaskStatus{State: t.state, Message: t.status, Timestamp: t.updated.UTC().Format(time.RFC3339Nano)},
		Artifacts: append([]a2a.Artifact(nil), t.artifacts...),
		History:   []a2a.Message{t.request},
	}
	if historyLength != nil && *historyLength == 0 {
		out.History = nil
	}
	if len(t.metadata) > 0 {
		out.Metadata = map[string]any{}
		for k, v := range t.metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// setState moves the task on; a terminal state is final and closes done.
func (t *task) setState(state, note string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a2a.Terminal(t.state) {
		return false
	}
	t.state = state
	if note != "" {
		t.status = agentMessage(note, t.id, t.contextID)
	}
	t.updated = time.Now()
	if a2a.Terminal(state) {
		t.finished = t.updated
		close(t.done)
	}
	return true
}

// note records progress without changing the state.
func (t *task) note(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a2a.Terminal(t.state) {
		return
	}
	t.status = agentMessage(text, t.id, t.contextID)
	t.updated = time.Now()
}

func agentMessage(text, taskID, contextID string) *a2a.Message {
	return &a2a.Message{
		Kind: "message", Role: "agent", Parts: []a2a.Part{a2a.TextPart(text)},
		MessageID: newID(), TaskID: taskID, ContextID: contextID,
	}
}

// store keeps tasks in memory and forgets finished ones after finishedTTL.
type store struct {
	mu    sync.Mutex
	tasks map[string]*task
}

func newStore() *store { return &store{tasks: map[string]*task{}} }

func (s *store) add(t *task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.id] = t
}

// get finds a task of the given agent.
func (s *store) get(agent, id string) *task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	if t == nil || t.agent != agent {
		return nil
	}
	return t
}

// sweep drops tasks finished longer than finishedTTL ago.
func (s *store) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tasks {
		t.mu.Lock()
		old := !t.finished.IsZero() && now.Sub(t.finished) > finishedTTL
		t.mu.Unlock()
		if old {
			delete(s.tasks, id)
		}
	}
}

// all lists the tasks (for shutdown).
func (s *store) all() []*task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // a version 4 UUID
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
