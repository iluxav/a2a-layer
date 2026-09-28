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

## What a task can do

Each Claude run is sealed off, so an agent acts only through the MCP servers you give it:

- no built-in tools: no shell, file access, or web fetch (`--tools ""`)
- only the agent's MCP servers (`--strict-mcp-config`), and only the tools listed for each;
  anything else is refused, never asked about (`--permission-mode dontAsk`)
- no user, project, or local settings, so no hooks or plugins (`--setting-sources ""`)
- a fresh, empty working directory per task, removed afterwards, and no saved session

The CLI never talks to the MCP servers directly. For each task, the layer connects to them
itself (sending the configured and forwarded headers) and serves the CLI a copy that lists only
the agent's allowed tools, at a private URL on its own port that disappears when the task ends.
This matters because a CLI loads the definition of every tool a server offers, whatever it is
allowed to call: a server with hundreds of tools would fill the model's context before the task
starts. It also keeps the keys out of the CLI's configuration, and it offers the servers no
interactive capabilities, so a gateway that asks a human before risky calls asks its own
console. An allowed tool a server does not offer is logged and noted on the task.

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
    secret: ${PM_SECRET}        # callers must send "Authorization: Bearer <secret>"
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

## Behind Delegent

[`examples/delegent-team.yaml`](examples/delegent-team.yaml) is a five-agent software team (PM,
designer, engineer, QA, DevOps) that plans in Linear, builds on GitHub, and deploys with Vercel,
every call going through a Delegent gateway.

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
   Keep the same seal: no built-in tools, only the job's MCP servers.
2. Register it: `func init() { Register("codex", NewCodex) }`.
3. Use it: `runners: {codex: {model: …}}` and `runner: codex` on an agent.

The server and config need no changes.

## Layout

- `cmd/a2a-layer` – the binary: flags, runners, HTTP server, shutdown
- `internal/config` – the YAML schema, `${NAME}` expansion, defaults and validation
- `internal/server` – A2A over HTTP: routing per agent, auth, tasks, running them
- `internal/mcpproxy` – each task's filtered, private view of its MCP servers
- `internal/runner` – the runner interface and the Claude Code implementation
- `internal/a2a` – the A2A wire types
