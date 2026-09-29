package server

import (
	"context"
	"sync"
	"time"
)

// conversation is one agent's memory of one A2A context: the directory its tasks run in and
// the CLI session they continue. Its tasks take turns: a session is continued by one task at
// a time, so the next one sees everything the previous one did.
type conversation struct {
	agent, contextID string
	idle             time.Duration
	turn             chan struct{} // held by the task running in the conversation

	// Guarded by turn (only its holder reads or writes them), except lastUsed and closed,
	// which the sweep reads under the store's lock.
	dir      string
	session  string // the runner's session id; "" until the first task finishes
	tasks    int    // tasks run in the current session
	lastUsed time.Time
	closed   bool
}

// conversations holds every live conversation, by agent and context.
type conversations struct {
	mu sync.Mutex
	m  map[string]*conversation
}

func newConversations() *conversations { return &conversations{m: map[string]*conversation{}} }

// acquire waits for the conversation's turn and returns it, creating the conversation on first
// use. A conversation the sweep ended while the caller waited is replaced by a fresh one.
func (cs *conversations) acquire(ctx context.Context, agent, contextID string, idle time.Duration) (*conversation, error) {
	for {
		cs.mu.Lock()
		key := agent + "\x00" + contextID
		c := cs.m[key]
		if c == nil {
			c = &conversation{agent: agent, contextID: contextID, idle: idle, turn: make(chan struct{}, 1), lastUsed: time.Now()}
			cs.m[key] = c
		}
		cs.mu.Unlock()
		select {
		case c.turn <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		cs.mu.Lock()
		closed := c.closed
		if !closed {
			c.lastUsed = time.Now()
		}
		cs.mu.Unlock()
		if !closed {
			return c, nil
		}
		<-c.turn
	}
}

// release ends the holder's turn.
func (cs *conversations) release(c *conversation) {
	cs.mu.Lock()
	c.lastUsed = time.Now()
	cs.mu.Unlock()
	<-c.turn
}

// sweep ends the conversations idle past their timeout (all of them with force) and returns
// them for cleanup. A conversation with a task in it is never ended.
func (cs *conversations) sweep(now time.Time, force bool) []*conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []*conversation
	for key, c := range cs.m {
		if !force && now.Sub(c.lastUsed) < c.idle {
			continue
		}
		select {
		case c.turn <- struct{}{}: // nobody is in it
		default:
			continue
		}
		c.closed = true
		delete(cs.m, key)
		<-c.turn
		out = append(out, c)
	}
	return out
}

// count is how many conversations are live (for tests and logs).
func (cs *conversations) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.m)
}
