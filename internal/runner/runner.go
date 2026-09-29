// Package runner runs one task on a command-line agent (a CLI such as claude) and returns
// what it said. Each CLI is one implementation of Runner, registered by type name; the
// server knows nothing about any particular CLI.
//
// Adding a CLI (codex, gemini, …) means writing a Runner that turns a Job into that CLI's
// flags and MCP config, runs it headless, and parses its output, then registering it in
// Register. Nothing else changes. A CLI that can continue a conversation reports its session
// id in Result.SessionID and resumes it when a later Job sets Resume (claude --resume; codex
// exec resume); one that stores sessions on disk also implements SessionForgetter.
package runner

import (
	"context"
	"errors"
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
	// MCP are the servers the agent may use.
	MCP []MCPServer
	// BuiltinTools are the CLI's own tools the agent may use, in the CLI's permission-rule
	// syntax (for claude: "Read", "Bash(lspci *)"), or AllBuiltinTools alone for every one of
	// them with every call allowed. Empty, the usual case, gives it none: it acts only
	// through MCP.
	BuiltinTools []string
	// WorkDir is the directory the CLI runs in: an empty scratch directory, or, for a job that
	// continues a conversation, the same directory every job of that conversation ran in.
	WorkDir string
	// Resume continues the CLI's earlier session with this id (a previous Result.SessionID of a
	// job run in the same WorkDir); empty starts a new session.
	Resume string
	// KeepSession saves the session so a later job can resume it. Without it the CLI keeps
	// nothing once the job ends.
	KeepSession bool
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

// AllBuiltinTools, as a job's only BuiltinTools entry, gives the agent all of the CLI's
// built-in tools with every call allowed, as if a person approved each one.
const AllBuiltinTools = "default"

// ErrSessionNotFound is returned when the session a job asked to resume no longer exists.
// Nothing ran: the job can be run again as a new session.
var ErrSessionNotFound = errors.New("the session to resume no longer exists")

// SessionForgetter is implemented by runners whose CLI stores sessions: ForgetSessions
// deletes every session the CLI kept for jobs run in workDir. Called when a conversation ends.
type SessionForgetter interface {
	ForgetSessions(workDir string) error
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
