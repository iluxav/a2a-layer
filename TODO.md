# TODO

Two ways to make the agents reachable from more clients. Most harnesses (Claude Code and the
Claude apps, ChatGPT and Codex, pi) call MCP servers but not A2A agents, so today they reach an
a2a-layer agent only through a gateway such as Delegent. And the A2A clients that do exist are
moving to A2A 1.0, while the layer speaks 0.3.

## 1. An MCP endpoint per agent (done)

Done as below; the README has a section on it. Verified from Claude Code and Codex as clients,
including a task that outlasts the wait (Claude Code then called `get_task` rather than asking
again). One addition: Claude Code shows a model only the structured result, not the text, so the
structured result also carries `next`, the advice to wait with `get_task`.

Serve each agent as a Streamable HTTP MCP server at `/<agent>/mcp`, next to its A2A endpoint, so
any MCP client can call it directly:

```
http://127.0.0.1:7300/pm        ← A2A (as now)
http://127.0.0.1:7300/pm/mcp    ← MCP: the same agent, as tools
```

### Tools

- One tool per skill, named by the skill's id (an agent without skills has one, named after it).
  Its description is the skill's description plus its examples; its input is
  `{"message": string, "context_id"?: string}`.
- `get_task {"task_id"}`: where a task is, and its answer once it has one.
- `cancel_task {"task_id"}`.

A call starts a task the way `message/send` does, then waits for it, but only up to a limit
(default 50s). MCP clients time tool calls out (Codex, for one, after 60s by default:
`tool_timeout_sec`), and an agent's task can take many minutes. A task that finishes in time
returns its answer. One that doesn't returns "still working" with its `task_id`, and the caller
polls `get_task`. This is the pattern the example team already follows through Delegent.

Every result also carries `structuredContent`: `{task_id, context_id, state, answer, status,
next}`. A caller that
sends `context_id` back continues the conversation, for an agent with `context.remember`.

### Details

- Auth: the agent's `secret`, as `Authorization: Bearer …`, same as A2A. Claude Code, Codex,
  Hermes and pi's MCP adapter can all send a static header. OAuth (which the MCP spec
  describes) is out of scope for now.
- `forward_headers` keep working: the tool handler gets the HTTP request's headers
  (`req.Extra.Header`) and passes them to the task as `send` does.
- Progress: if the client sent a progress token, send the task's notes ("calling
  linear__get_issue") as MCP progress notifications while the call waits.
- Routing: `POST /{agent}/` currently sends every path under an agent to the JSON-RPC handler.
  Register `/{agent}/mcp` (GET, POST and DELETE, for Streamable HTTP) explicitly; the more
  specific pattern wins.
- This is not the per-task MCP proxy (`/mcp-proxy/…`), which serves an agent its own servers.
  Keep the two apart.

### Tasks

- [x] `internal/server/mcpserve.go`: build an `mcp.Server` per agent from its skills, with its
      handlers calling `s.start`, `s.tasks.get` and `t.cancel` directly, not over HTTP.
- [x] Bounded wait, and "still working" results; the wait is `mcp_wait` (default 50s).
- [x] Bearer auth on `/{agent}/mcp`, and forwarded headers from the MCP request.
- [x] Progress notifications while a call waits.
- [x] Config: served by default for every agent (see Open questions).
- [x] Dashboard: each agent's MCP URL, with copy-and-paste setup for Claude Code and Codex (pi's
      MCP adapter has its own config file; not covered).
- [x] Tests: an MCP client (go-sdk) against the endpoint: list the tools, call one that
      finishes in time and one that doesn't, then poll it; continue a context; a wrong secret is
      refused; forwarded headers arrive upstream.
- [x] Try it for real from Claude Code and Codex.
- [x] README: a section on calling agents over MCP.

### Open questions

- ~~On by default for every agent, or `mcp: true` per agent?~~ On by default: it adds no access
  that the A2A endpoint doesn't already give (same auth, same tasks).
- Cloud clients (ChatGPT connectors, the Claude apps) can't reach `127.0.0.1`; they need a
  public HTTPS URL, and ChatGPT wants OAuth or no auth. Leave that to a tunnel or proxy for
  now.

## 2. A2A 1.0 alongside 0.3

A2A 1.0 (April 2026) changed the wire format. The layer speaks 0.3, so a 1.0 client, such as
Hermes Agent's A2A plugin, is refused. Gemini CLI's remote subagents still send 0.3
(`message/send`), so they must keep working.

| | 0.3 (today) | 1.0 |
|---|---|---|
| Methods | `message/send`, `tasks/get`, `tasks/cancel` | `SendMessage`, `GetTask`, `CancelTask` (and `SendStreamingMessage`, `SubscribeToTask`, `ListTasks`, `GetExtendedAgentCard`, push config) |
| Version | implied | `A2A-Version: 1.0` header; the server rejects versions it does not support |
| Task states | `completed`, `failed`, … | `TASK_STATE_COMPLETED`, `TASK_STATE_FAILED`, … |
| Roles | `user`, `agent` | `ROLE_USER`, `ROLE_AGENT` |
| Parts | `{"kind": "text", "text": …}` | no `kind`: the member present says what it is (`text`, `url`, `data`), plus `mediaType` and `filename` |
| Card | `url`, `preferredTransport`, `protocolVersion` at the top | `supportedInterfaces: [{url, protocolBinding, protocolVersion}]`; `capabilities.extendedAgentCard` |

### Approach

Serve both versions at the same URL. Keep the tasks, conversations and runners
version-neutral, as they are, and translate only at the edge:

- Pick the version from the `A2A-Version` header: `1.0` goes to the 1.0 handler; `0.3`, or no
  header, goes to today's handler. Reject any other version with the error 1.0 defines. Do not
  guess the version from the method name.
- `internal/a2a`: add the 1.0 types (or a `v1` package), and convert both ways between them and
  the internal task, message and part.
- The card: one card that 0.3 clients (top-level `url` and `protocolVersion`) and 1.0 clients
  (`supportedInterfaces`, one entry per version) both read. Check that each version's official
  SDK accepts the combined card; if not, choose by the `A2A-Version` header on the card request.

### Tasks

- [ ] 1.0 wire types and converters in `internal/a2a`, with round-trip tests.
- [ ] Dispatch on `A2A-Version` in `rpc`; the 1.0 handler for `SendMessage`, `GetTask`,
      `CancelTask`; other 1.0 methods answer "method not found", as 0.3 does now.
- [ ] Parts: accept 1.0 text and data parts; report a file part (`url`) as unsupported, as now.
- [ ] The combined agent card, checked against both SDKs.
- [ ] Tests: the same scenarios in both versions (send, poll, cancel, context, errors).
- [ ] Try it for real: Gemini CLI (0.3) and Hermes Agent's `a2a` toolset (1.0) against the
      layer.
- [ ] README: which versions are served, and how a client picks.

### Later

- Streaming: `message/stream` (0.3) and `SendStreamingMessage` / `SubscribeToTask` (1.0), over
  SSE. The progress notes are already there; today callers have to poll for them. The card
  says `streaming: false` until then.

## Also

- [ ] Check today's 0.3 endpoint from Gemini CLI (`kind: remote`, `agent_card_url:
      http://127.0.0.1:7300/<agent>/.well-known/agent-card.json`). It should work, but nobody
      has tried it.

References: [What's new in A2A 1.0](https://a2a-protocol.org/latest/whats-new-v1/),
[Gemini CLI remote subagents](https://geminicli.com/docs/core/remote-agents/),
[Hermes Agent A2A](https://hermes-agent.nousresearch.com/docs/user-guide/messaging/a2a).
