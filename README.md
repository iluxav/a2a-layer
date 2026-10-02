# a2a-layer

Serve command-line agents as [A2A](https://a2a-protocol.org) agents. One small Go binary runs next
to a CLI such as Claude Code and exposes each agent you define in a YAML file as its own A2A
endpoint on one port:

```
http://127.0.0.1:7300/pm          ← JSON-RPC (message/send, tasks/get, tasks/cancel)
http://127.0.0.1:7300/pm/.well-known/agent-card.json
http://127.0.0.1:7300/pm/mcp      ← the same agent as an MCP server, for MCP-only clients
http://127.0.0.1:7300/engineer
http://127.0.0.1:7300/                ← index of the agents
```

A task runs the CLI headless with the agent's instructions, model, and MCP servers, and returns
what it said. An agent runs on Claude Code (the default) or Codex; other CLIs (Gemini, …) plug in
behind the same interface.

## Quick start

```sh
make build                        # ./a2a-layer
make run CONFIG=agents.yaml       # build and serve (default config: examples/delegent-team.yaml)
make install                      # copy into /usr/local/bin (BINDIR=… to change; sudo only if needed)
make uninstall

a2a-layer -config agents.yaml -check   # validate and list the agents, then exit
```

On start it prints the full path of the config it loaded, then one line per agent with its URL.

A minimal config:

```yaml
listen: 127.0.0.1:7300

agents:
  helper:
    description: Answers questions about our docs.
    model: haiku
    instructions: Answer briefly, citing the page you used.
    mcp:
      docs:
        url: https://docs.example.com/mcp
```

Call it:

```sh
curl -s localhost:7300/helper -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":1,"method":"message/send",
  "params":{"message":{"kind":"message","role":"user","messageId":"m1",
    "parts":[{"kind":"text","text":"How do I rotate a key?"}]}}}'
```

Without `configuration.blocking: false` the call waits for the answer; with it, you get the task
back at once and poll `tasks/get`.

## Calling agents over MCP

Most harnesses (Claude Code, Codex, ChatGPT, pi) call MCP servers but not A2A agents, so every
agent is also a Streamable HTTP MCP server at `/<agent>/mcp`. Each of its skills is a tool taking
`{message, context_id?}`, and two more tools follow a task: `get_task {task_id}` and
`cancel_task {task_id}`.

```sh
claude mcp add --transport http pm http://127.0.0.1:7300/pm/mcp --header "Authorization: Bearer $PM_SECRET"
codex mcp add pm --url http://127.0.0.1:7300/pm/mcp --bearer-token-env-var PM_SECRET
```

A call starts a task exactly as `message/send` does (the same instructions, MCP servers,
forwarded headers and conversations; A2A's `tasks/get` sees it too) and waits for it, but only
up to `mcp_wait` (default 50s), since MCP clients time tool calls out and a task can take many
minutes. A task that finishes in time returns its answer. One that does not returns
`state: working` with its `task_id`, and `get_task` waits for it again. Every result carries
`{task_id, context_id, state, answer, status, next}` as structured content; pass `context_id`
back to continue a conversation with an agent that remembers. A client that asks for progress
gets the task's notes ("calling linear__get_issue") as progress notifications.

The endpoint takes the agent's `secret` as a bearer token, like its A2A endpoint. The dashboard
shows each agent's commands. Cloud clients (ChatGPT connectors, the Claude apps) need a public
HTTPS URL to reach it; behind a reverse proxy, set `public_url`, and the endpoint accepts requests
under that host name (otherwise it refuses any host but a loopback one, against DNS rebinding).

## Dashboard

```sh
a2a-layer dashboard -config agents.yaml    # http://127.0.0.1:7301 (-listen to change)
make dashboard CONFIG=agents.yaml
```

A web UI for the config file, served from the same binary (its pages, stylesheet and htmx are
embedded):

- **Agents**: add, edit, rename and delete agents: card, instructions, CLI and limits, secret,
  conversations, MCP servers with their headers and allowed tools, built-in tools, and skills.
  The model is picked from those the agent's CLI offers (`codex debug models` for Codex; the
  aliases for Claude), or typed.
- **Settings**: listen address, public URL, env file, work dir and the common instructions, and
  (optional) how each CLI is run: its command, extra arguments and environment. It also shows
  whether each CLI is found.
- **Playground**: chat with an agent and follow each task as it runs: progress, answer
  (rendered from Markdown), turns, tokens and cost. Each conversation is one thread, listed on
  the side; an agent that remembers is continued by replying in its thread, one that does not
  starts a new conversation with every message. Conversations are kept while the dashboard
  runs (the last 50). The agents run inside the dashboard, exactly as a2a-layer would run them,
  from the config as it is now; a2a-layer need not be running.

Every change is written back to the file right away. Only the entry you changed is rewritten:
the rest of the file keeps its comments, layout and `${NAME}` references, and the forms show
values as written, so a secret stays `${PM_SECRET}`. A change that would break the config (an
unset variable, an agent without a description, a CLI that a2a-layer does not run) is refused with
the reason; problems the file already had are shown but do not block other changes. The file
need not exist: the first change creates it. a2a-layer reads the config when it starts, so
restart it to serve what you changed.

The dashboard has no login and can change the config and run agents, so keep it on a loopback
address. It refuses cross-site requests and requests for any host but localhost.

## What a task can do

Each Claude run is sealed off, so an agent acts only through the MCP servers you give it:

- no built-in tools: no shell, file access, or web fetch (`--tools ""`), unless the agent lists
  some (see Built-in tools, below)
- only the agent's MCP servers (`--strict-mcp-config`), and only the tools listed for each;
  anything else is refused, never asked about (`--permission-mode dontAsk`)
- no user, project, or local settings, so no hooks or plugins (`--setting-sources ""`)
- a fresh, empty working directory per task, removed afterwards, and no saved session (unless
  the agent remembers conversations, below)

The CLI never talks to the MCP servers directly. For each task, the layer connects to them
itself (sending the configured and forwarded headers) and serves the CLI a copy that lists only
the agent's allowed tools, at a private URL on its own port that disappears when the task ends.
This matters because a CLI loads the definition of every tool a server offers, whatever it is
allowed to call: a server with hundreds of tools would fill the model's context before the task
starts. It also keeps the keys out of the CLI's configuration, and it offers the servers no
interactive capabilities, so a gateway that asks a human before risky calls asks its own
console. An allowed tool a server does not offer is logged and noted on the task.

### Built-in tools

An agent that should work on the machine it runs on (read its files, run `lspci`) can be given
the CLI's own tools with `builtin_tools`. Without it an agent has no tools but its MCP servers,
and asked to look at the machine it can only say what it would run. Listing whole tools is not
enough on its own terms, either: a headless CLI has nobody to approve a call, so the layer also
has to say which calls are allowed.

- `builtin_tools: [default]` gives every built-in tool, with every call allowed, as if you
  approved each one (for Claude: `--tools default --permission-mode bypassPermissions`).
- Or list permission rules in the CLI's syntax, for Claude a whole tool (`Bash`, `Read`,
  `WebFetch`) or a narrower rule (`Bash(lspci *)`, which allows only commands starting with
  `lspci`). The CLI gets those tools and may make only the calls the rules allow.

Either way the agent's MCP servers still offer only their allowed tools. Built-in tools act on
this machine as the user a2a-layer runs as, so give an agent that has them a `secret`.

```yaml
agents:
  pc:
    description: Answers questions about this machine.
    builtin_tools: [default]
  gpu:
    description: Reports on this machine's GPUs, and nothing else.
    builtin_tools: ["Bash(lspci *)", "Bash(nvidia-smi *)", "Bash(grep *)"]
  pm:
    description: Plans projects.     # no builtin_tools: sealed, only its MCP servers
```

The dashboard offers the same choice on the agent form: none, all, or only the rules you list.

### Conversations

By default every task starts from nothing: the agent knows only the message it was sent. With
`context: {remember: true}`, an agent remembers earlier tasks of the same A2A `contextId`. The
first task of a context starts a CLI session in a directory of its own; each later task of that
context resumes the session there, so it sees what the earlier ones asked, did and answered.
The server answers every task with its `contextId`; a caller that sends it back continues the
conversation, and one that sends none starts a new one.

- Contexts never share anything: each has its own session and directory.
- A context's tasks run one at a time (a session is continued by one task at a time); a second
  task waits as `submitted`. Other contexts are not held up, within `max_parallel`.
- After `max_tasks` tasks the session starts afresh, so the prompt it carries stays bounded.
- A conversation unused for `idle_timeout` ends: its session and directory are deleted. They
  are also deleted on shutdown, since conversations are kept in memory.

A caller that knows a context id can continue that conversation, so give remembering agents a
`secret`, and let a gateway in front of them choose the context ids.

Claude runs under whatever login the CLI already has. Using your own Claude subscription for
your own agents on your own machine is ordinary Claude Code use; Anthropic does not allow
offering claude.ai login or subscription limits in a product for other people, so anything you
ship to others should run on an API key.

## Config reference

```yaml
listen: 127.0.0.1:7300          # where to serve (default)
public_url: https://agents.example.com   # base URL in the cards (default http://<listen>)
env_file: .env                  # loaded before ${NAME} expansion, relative to this file
work_dir: /var/tmp/a2a          # where task scratch dirs go (default: system temp)
common_instructions: |          # appended to every agent's instructions
  …
mcp_wait: 50s                   # how long an MCP tool call waits for its task (default 50s)

cli:                            # optional: how a CLI is run on this machine, for all its agents
  claude:                       # or codex
    command: /opt/claude/bin/claude   # executable (default: the CLI's name, on PATH)
    args: []                    # extra arguments on every run
    env:                        # extra environment, e.g. another login
      CLAUDE_CONFIG_DIR: /home/me/.claude-work

agents:
  pm:                           # served at /pm; lowercase letters, digits, - and _
    description: …              # required; shown on the card
    version: 1.0.0
    instructions: |             # the role, appended to the CLI's system prompt
      …
    skills:                     # the card's skills (default: one, named after the agent)
      - id: plan_project
        name: Plan a project
        description: …
        tags: [planning]
        examples: ["…"]
    cli: claude                 # the program its tasks run on: claude (default) or codex
    model: sonnet               # the CLI's model name or alias (default: the CLI's default)
    max_turns: 60               # default 30
    timeout: 45m                # default 15m
    max_parallel: 2             # tasks beyond this wait their turn (default 1)
    context:                    # remember earlier tasks of the same A2A contextId (see Conversations)
      remember: true            # default false: every task starts from nothing
      idle_timeout: 1h          # a conversation unused this long ends (default 1h)
      max_tasks: 20             # then its session starts afresh (default 20)
    secret: ${PM_SECRET}        # callers must send "Authorization: Bearer <secret>"
    builtin_tools: [Read]       # the CLI's own tools: rules, or [default] for all (default none)
    mcp:
      gateway:                  # a Streamable HTTP MCP server, by name
        url: ${GATEWAY_URL}
        headers:
          Authorization: Bearer ${PM_KEY}
        forward_headers: [X-Delegent-Session]   # copied from the A2A request onto every call
        tools: [linear__save_issue, linear__get_issue]   # empty: all of the server's tools
```

Any value can use `${NAME}` from the environment or `env_file`; a name that is not set is an
error. A bare `$` (as in "$9 a month") is left alone, and so are comments. Unknown keys are errors,
so a typo does not silently change behaviour.

Each task's metadata reports the CLI, model, turns, tokens, cost and duration; the server logs
one line per task start and finish.

## Example: a team behind a consent gateway

a2a-layer works with any MCP servers. [`examples/delegent-team.yaml`](examples/delegent-team.yaml)
is one setup: a five-agent software team (PM, designer, engineer, QA, DevOps) that plans in
Linear, builds on GitHub, and deploys with Vercel, with every call going through one MCP gateway
(Delegent) that holds the service credentials and asks a human before risky actions.

1. Copy `examples/.env.example` to `examples/.env`, set `DELEGENT_URL`, and put a long random
   string in each `<NAME>_SECRET`. Leave the keys empty for now.
2. Start the layer: `./a2a-layer -config examples/delegent-team.yaml`.
3. In Delegent, add each agent as an A2A agent at `http://127.0.0.1:7300/<name>` with its secret
   as the token, and mint a key for it. Put the keys into `.env` as `<NAME>_KEY` and restart the
   layer.

`forward_headers: [X-Delegent-Session]` is what chains the hops: the session Delegent sent to the
agent rides along on every call the agent makes back through Delegent, so the gateway shows
`you → pm → engineer → github` as one run.

## Codex

Agents can run on the Codex CLI instead of Claude Code, under whatever login it has:

```yaml
agents:
  reviewer:
    cli: codex
    model: …                    # a model your codex login offers; empty uses codex's default
    description: …
```

A Codex task is sealed the same way (`codex exec --json`): your `~/.codex/config.toml`, rules,
plugins and apps are ignored; the shell is switched off, the sandbox is read-only and web search
is disabled, so the agent acts only through its MCP servers, whose allowed tools it calls without
asking. Conversations work as with Claude (`codex exec resume`), and an ended one's session files
are deleted. The differences:

- `builtin_tools` takes `[default]` or nothing. `[default]` gives every Codex tool with no
  sandbox and nothing asked (`--dangerously-bypass-approvals-and-sandbox`). Codex has no
  permission rules like `Bash(lspci *)`: a Codex agent given some is refused when
  a2a-layer starts, by `-check`, and by the dashboard, which offers Codex only None or All.
- Codex has no turn limit of its own: the layer stops a task after `max_turns` tool calls.
- Codex reports tokens but not cost.

## Adding another CLI

Inside a2a-layer, a runner turns a job into one headless run of a CLI. To add a CLI (see
`claude.go` and `codex.go`):

1. Write `internal/runner/<cli>.go` implementing

   ```go
   type Runner interface {
       Run(ctx context.Context, job Job, progress Progress) (Result, error)
   }
   ```

   A `Job` carries the prompt, instructions, model, turn limit, work directory, and the MCP
   servers with their headers and allowed tools; translate those into the CLI's flags and MCP
   config, run it in `job.WorkDir`, report progress as it goes, and parse its final answer.
   Keep the same seal: only the job's MCP servers, and no built-in tools but those the job's
   `BuiltinTools` rules allow.
   For conversations, report the CLI's session (or thread) id in `Result.SessionID`, and when a
   job sets `Resume`, continue that session; return
   `ErrSessionNotFound` if it no longer exists. `KeepSession` says whether to save the session
   at all. A CLI that stores sessions on disk also implements `SessionForgetter`, so an ended
   conversation leaves nothing behind. A CLI with permission rules for its built-in tools
   implements `Describer` (`Info() Info` with `ToolRules` and example rules); one without takes
   only `[default]` or nothing. One that can list its models implements `ModelLister`, and the
   dashboard offers them.
2. Register it: `func init() { Register("gemini", NewGemini) }`.
3. Use it: `cli: gemini` on an agent.

The server and config need no changes.

## Layout

- `cmd/a2a-layer` – the binary: flags, HTTP server, shutdown, the `dashboard` command
- `internal/config` – the YAML schema, `${NAME}` expansion, defaults and validation
- `internal/server` – A2A over HTTP: routing per agent, auth, tasks, running them; each agent's
  MCP endpoint
- `internal/dashboard` – the web UI: config editing (entry by entry), the playground, embedded
  templates and assets
- `internal/mcpproxy` – each task's filtered, private view of its MCP servers
- `internal/runner` – running a task on a CLI: the interface, and the Claude Code and Codex
  implementations
- `internal/a2a` – the A2A wire types
