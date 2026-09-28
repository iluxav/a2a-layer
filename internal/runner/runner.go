// Package runner runs one task on a command-line agent (a CLI such as claude) and returns
// what it said. Each CLI is one implementation of Runner, registered by type name; the
// server knows nothing about any particular CLI.
//
// Adding a CLI (codex, gemini, …) means writing a Runner that turns a Job into that CLI's
// flags and MCP config, runs it headless, and parses its output, then registering it in
// Register. Nothing else changes.
package runner

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Job is one task, in terms any CLI can be told about.
type Job struct {
	// Prompt is the caller's request.
	Prompt string
	// Instructions describe the agent's role; they go into the system prompt.
	Instructions string
	// Model is the CLI's model name or alias; empty uses the CLI's default.
	Model string
	// MaxTurns bounds the agent's loop.
	MaxTurns int
	// MCP are the only servers the agent may use; it gets no other tools.
	MCP []MCPServer
	// WorkDir is an empty scratch directory the CLI runs in.
	WorkDir string
}

// MCPServer is one Streamable HTTP MCP server, with the headers to send on every call.
type MCPServer struct {
	Name    string
	URL     string
	Headers map[string]string
	// Tools limits the agent to these tools of the server; empty allows all.
	Tools []string
}

// Result is what the CLI finished with.
type Result struct {
	Text string
	// IsError is set when the CLI ran but the agent did not finish its task (for example it
	// hit its turn limit); Text then says why.
	IsError      bool
	Turns        int
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	SessionID    string
}

// Progress receives short, human-readable notes while a task runs ("calling linear__get_issue").
type Progress func(note string)

// Runner runs jobs on one CLI.
type Runner interface {
	Run(ctx context.Context, job Job, progress Progress) (Result, error)
}

// Options configure a runner from the config file.
type Options struct {
	// Command is the executable; empty uses the implementation's default.
	Command string
	// Args are extra arguments added to every run.
	Args []string
	// Env is added to the CLI's environment.
	Env map[string]string
	// KillGrace is how long a canceled CLI gets to exit before it is killed.
	KillGrace time.Duration
}

// Factory builds a Runner from its options.
type Factory func(Options) Runner

var factories = map[string]Factory{}

// Register makes an implementation available under a type name.
func Register(typ string, f Factory) { factories[typ] = f }

// New builds the runner of the given type.
func New(typ string, o Options) (Runner, error) {
	f, ok := factories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown runner type %q (available: %v)", typ, Types())
	}
	return f(o), nil
}

// Types lists the registered implementations.
func Types() []string {
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
