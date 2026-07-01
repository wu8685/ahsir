# AHSIR Examples

Five self-contained worked examples, increasing in complexity. Pick where to start based on what you want to learn.

| Example | Demonstrates | Read order |
|---|---|---|
| [`simple/`](./simple/) | The smallest possible end-to-end path: one agent, one curl, one answer | **Start here** to verify your build + API key |
| [`session-reuse/`](./session-reuse/) | Conversation memory across multiple curls via stable `contextId` — in-process reuse, cross-restart resume, and self-healing on SIGKILL | Next, to see what `SessionPool` actually does |
| [`recovery-continuation/`](./recovery-continuation/) | Scheduler ledger, supervised restart, continuation prompt, and `sessions.json` inactive mapping retention | Then, to see failure recovery mechanics |
| [`multi-agent/`](./multi-agent/) | Two-agent setup (Student delegates to Teacher via `---A2A_CALL---`), filesystem access, scheduler gateway, Claude/Codex plugin + CLI integration | Then, for the full multi-agent + gateway story |
| [`roundtable/`](./roundtable/) | Multi-agent **group chat**: advocate + critic debate in one shared thread, `@`-mention turn-taking, operator/agent moderator, incremental relay, web console | Last, for visible multi-agent collaboration |

Each subdirectory is self-contained: own `ahsir.yaml`, agent card(s), and walkthrough README. Pick one and read its README from top to bottom — they don't share state, can be run independently, and use the same default ports (only run one at a time).

## Common prerequisites

All three examples need the same baseline:

```bash
# 1. Build the binaries (from repo root)
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent

# 2. Set your Anthropic API key (the bundled cards use Anthropic / claude-opus-4-8)
export ANTHROPIC_AUTH_TOKEN=<your-anthropic-key>

# 3. Make sure `claude` is on PATH
which claude
```

Then `cd` into whichever subdirectory you want and follow its README.

## Run the tests

The integration tests live with the `multi-agent/` example (they exercise the delegation path):

```bash
go test ./example/multi-agent/ -v
```

The tests use mocks for the LLM, so they do **not** require `ANTHROPIC_AUTH_TOKEN` or a real `claude` CLI.
