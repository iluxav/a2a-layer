// Package a2a holds the Agent-to-Agent (A2A) protocol wire types this server speaks: the
// JSON-RPC 0.3 line (message/send, tasks/get, tasks/cancel) with its Agent Card, Task, Message
// and Part objects. Only what a text-in, text-out agent needs is modelled.
package a2a

import (
	"encoding/json"
	"strings"
)

// ProtocolVersion is the A2A version the cards advertise.
const ProtocolVersion = "0.3.0"

// Task states.
const (
	StateSubmitted = "submitted"
	StateWorking   = "working"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Terminal reports whether a task in this state is finished.
func Terminal(state string) bool {
	return state == StateCompleted || state == StateFailed || state == StateCanceled
}

// JSON-RPC and A2A error codes.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternalError      = -32603
	CodeTaskNotFound       = -32001
	CodeTaskNotCancelable  = -32002
	CodeUnsupportedContent = -32005
)

// AgentCard is what an agent publishes at <agent>/.well-known/agent-card.json.
type AgentCard struct {
	Name               string                    `json:"name"`
	Description        string                    `json:"description"`
	URL                string                    `json:"url"`
	Version            string                    `json:"version"`
	ProtocolVersion    string                    `json:"protocolVersion"`
	PreferredTransport string                    `json:"preferredTransport"`
	Capabilities       Capabilities              `json:"capabilities"`
	DefaultInputModes  []string                  `json:"defaultInputModes"`
	DefaultOutputModes []string                  `json:"defaultOutputModes"`
	Skills             []Skill                   `json:"skills"`
	SecuritySchemes    map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	Security           []map[string][]string     `json:"security,omitempty"`
}

// Capabilities are the optional protocol features an agent supports.
type Capabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// Skill is one thing an agent offers, as its card describes it.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
}

// SecurityScheme describes how a caller authenticates (here: an HTTP bearer token).
type SecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme,omitempty"`
}

// Part is one piece of a message or artifact.
type Part struct {
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Data     any            `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TextPart is a text part.
func TextPart(s string) Part { return Part{Kind: "text", Text: s} }

// Message is one turn of a conversation.
type Message struct {
	Kind      string         `json:"kind"`
	Role      string         `json:"role"`
	Parts     []Part         `json:"parts"`
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Text flattens a message's parts: text as is, data as JSON, one part per line.
func (m Message) Text() string {
	var out []string
	for _, p := range m.Parts {
		switch {
		case p.Text != "":
			out = append(out, p.Text)
		case p.Data != nil:
			if b, err := json.Marshal(p.Data); err == nil {
				out = append(out, string(b))
			}
		}
	}
	return strings.Join(out, "\n")
}

// TaskStatus is where a task stands, with the agent's latest word on it.
type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

// Artifact is an output of a task.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// Task is the unit of work message/send starts and tasks/get reports on.
type Task struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// SendParams are message/send's parameters.
type SendParams struct {
	Message       Message        `json:"message"`
	Configuration *SendConfig    `json:"configuration,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// SendConfig tunes one message/send call.
type SendConfig struct {
	// Blocking false asks for the task back at once, to be polled with tasks/get. Absent
	// or true waits for the task to finish.
	Blocking *bool `json:"blocking,omitempty"`
}

// TaskParams are tasks/get's and tasks/cancel's parameters.
type TaskParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response: a result or an error.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }
