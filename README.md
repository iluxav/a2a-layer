# a2a-layer

Serve command-line agents as [A2A](https://a2a-protocol.org) agents. One small Go binary runs next
to a CLI such as Claude Code and exposes each agent you define in a YAML file as its own A2A
endpoint on one port:

```
http://127.0.0.1:7300/pm          ← JSON-RPC (message/send, tasks/get, tasks/cancel)
http://127.0.0.1:7300/pm/.well-known/agent-card.json
http://127.0.0.1:7300/engineer
http://127.0.0.1:7300/                ← index of the agents
```

A task runs the CLI headless with the agent's instructions, model, and MCP servers, and returns
what it said. Claude Code is the first runner; other CLIs (Codex, Gemini, …) plug in behind the
same interface.

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
runners:
  claude:
    model: haiku

agents:
  helper:
    description: Answers questions about our docs.
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

## Dashboard

```sh
a2a-layer dashboard -config agents.yaml    # http://127.0.0.1:7301 (-listen to change)
make dashboard CONFIG=agents.yaml
```

A web UI for the config file, served from the same binary (its pages, stylesheet and htmx are
embedded):

- **Agents**: add, edit, rename and delete agents: card, instructions, runner and limits,
  secret, conversations, MCP servers with their headers and allowed tools, and skills.
- **Runners**: define the CLIs agents run on, with their defaults; shows whether each command is
  on `PATH` and which agents use it.
- **Settings**: listen address, public URL, env file, work dir and the common instructions.
- **Playground**: send an agent a message and follow its task as it runs: progress, answer,
  turns, tokens and cost. An agent that remembers can be continued in the same conversation.
  The agents run inside the dashboard, exactly as a2a-layer would run them, from the config as
  it is now; a2a-layer need not be running.

Every change is written back to the file right away. Only the entry you changed is rewritten:
the rest of the file keeps its comments, layout and `${NAME}` references, and the forms show
values as written, so a secret stays `${PM_SECRET}`. A change that would break the config (an
unset variable, an agent without a description, a runner that does not exist) is refused with
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
```

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

runners:                        # the CLIs tasks run on, by name
  claude:
    type: claude                # implementation (default: the runner's name)
    command: claude             # executable (default: the implementation's usual name)
    model: haiku                # defaults an agent can override
    max_turns: 30
    timeout: 15m
    args: []                    # extra CLI arguments on every run
    env: {}                     # extra environment for the CLI

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
    runner: claude              # default: the only runner configured
    model: sonnet
    max_turns: 60
    timeout: 45m
    max_parallel: 2             # tasks beyond this wait their turn (default 1)
    context:                    # remember earlier tasks of the same A2A contextId (see Conversations)
      remember: true            # default false: every task starts from nothing
      idle_timeout: 1h          # a conversation unused this long ends (default 1h)
      max_tasks: 20             # then its session starts afresh (default 20)
    secret: ${PM_SECRET}        # callers must send "Authorization: Bearer <secret>"
    builtin_tools: [Read]       # the CLI's own tools it may use: rules, or [default] for all (see Built-in tools)
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

Each task's metadata reports the runner, model, turns, tokens, cost and duration; the server logs
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

## Adding another CLI

A runner turns a job into one headless run of a CLI. To add one (say Codex):

1. Write `internal/runner/codex.go` implementing

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
   job sets `Resume`, continue that session (for Codex, `codex exec resume <id>`); return
   `ErrSessionNotFound` if it no longer exists. `KeepSession` says whether to save the session
   at all. A CLI that stores sessions on disk also implements `SessionForgetter`, so an ended
   conversation leaves nothing behind.
2. Register it: `func init() { Register("codex", NewCodex) }`.
3. Use it: `runners: {codex: {model: …}}` and `runner: codex` on an agent.

The server and config need no changes.

## Layout

- `cmd/a2a-layer` – the binary: flags, runners, HTTP server, shutdown, the `dashboard` command
- `internal/config` – the YAML schema, `${NAME}` expansion, defaults and validation
- `internal/server` – A2A over HTTP: routing per agent, auth, tasks, running them
- `internal/dashboard` – the web UI: config editing (entry by entry), the playground, embedded
  templates and assets
- `internal/mcpproxy` – each task's filtered, private view of its MCP servers
- `internal/runner` – the runner interface and the Claude Code implementation
- `internal/a2a` – the A2A wire types
