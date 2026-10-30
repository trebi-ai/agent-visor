# agent-visor

agent-visor keeps agent and terminal sessions alive and lets you attach to them. It does not sandbox them.

It is a Go library and a process. It sits one layer above [agent-wire](https://github.com/trebi-ai/agent-wire). agent-wire drives one vendor CLI over its structured protocol and keeps no state. agent-visor holds many sessions on one machine, keeps their events in a log, and survives a restart of the product that uses it.

Status: no code yet.

## What it does

- Starts agent sessions over agent-wire, vendor TUIs in a PTY, shells, and scripts.
- Keeps each session in a holder process, so a session survives a visor upgrade.
- Keeps an event log with a sequence number. A client that connects again reads the events that it missed.
- Lets a client attach to a session as a terminal or as a chat view, and hand a session over between the two.
- Finds projects from paths and manages workdirs: the main checkout, git worktrees, and plain folders.
- Detects, installs, updates, and logs in to the harnesses (Claude Code, Codex, and others), and lists their models.
- Takes hook calls from sessions that a user starts outside the visor, and shows them as observed sessions.
- Upgrades itself when no session needs it.

## How a product uses it

Each product runs its own visor. Each visor has its own home, socket, harness installs, and logins. The product talks to the visor over a Unix socket with `Dial`, or it runs a `Visor` in its own process for tests.

```go
c, err := agentvisor.Ensure(ctx, svc) // install or start the service, then dial
s, err := c.Start(ctx, agentvisor.StartSpec{Harness: "claude", WorkingDir: dir, Prompt: "..."})
stream, err := c.Events(ctx, agentvisor.Filter{Session: s.ID}, 0)
```

The generic CLI `agent-visor` uses the same API.

## Platforms

macOS and Linux. Windows is a later step.

## License

MIT
