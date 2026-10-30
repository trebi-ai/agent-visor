# CLAUDE.md

Instructions for humans and coding agents working in this repository.

agent-visor is a public Go library and a process. It keeps agent and terminal sessions alive on one machine and lets a product attach to them. It does not sandbox them. A product imports it as a library, or talks to it over its socket.

This file is the design contract until the code lands. When this file and the code disagree, check the code, then fix this file.

Prefer types that own behavior. Prefer fewer, denser files. Prefer explicit wiring over package-level state. Do not make `util` packages, one-function files, or interfaces that have a single implementation.

## Concepts

- **Visor**: one process (or one in-process `Visor`) for each product on a machine. It has a home folder, a socket, a SQLite store (`visor.db`), and an event log.
- **Project**: a repository or a folder. The visor finds projects from paths. There is no call to add a project.
- **Workdir**: a place to work in a project: the main checkout, a git worktree, or a plain folder. `EnsureWorkdir` finds it by key, or makes it once.
- **Session**: one agent, shell, or script in a workdir. Kinds: `wire` (agent-wire driver), `pty` (vendor TUI or shell in a PTY), `observed` (started outside the visor, seen through hooks), and the kinds of optional backends.
- **Backend**: hosts sessions of one kind. `wire` and `pty` are built in. `Caps` says what a backend can do.
- **Holder**: a small process (`agent-visor hold`) that owns the PTY or the stdio pipes of one session, so the session survives a visor restart.
- **Harness**: a vendor agent CLI (Claude Code, Codex, and others). The visor detects, installs, updates, and logs in to it, and lists its models. Each visor has its own installs and logins.
- **Event**: one entry in the event log, with a global sequence number. A client reads the events after the last sequence number that it saw.
- **Quiet upgrade**: the visor replaces itself when no session, login, or attach client needs it.

## Boundaries

Three layers. Keep each fact in its layer.

| Layer | Owns | Does not own |
|---|---|---|
| Product | Its own objects and their state, task policy, concurrency caps, idle and duration limits, the UI, secrets resolution | Agent processes, vendor protocols, harness installs |
| agent-visor | Projects, workdirs, live sessions of all kinds, the event log with sequence numbers, open permissions and questions, attach streams, handover, hooks intake, harness inventory, installs, updates, login, auth mode, models | Vendor protocol details, product policy |
| agent-wire | Vendor knowledge with no state: protocols, launch flags, detect, version floors, auth probes, install recipes, login recipes, model tables, screen classifiers (`agent-wire/tui`) | Files on disk, schedules, policy |

- Vendor knowledge goes into agent-wire first, then agent-visor uses it. Do not add a vendor flag, a screen pattern, or a model table here.
- agent-visor is product neutral. Do not put a product name, a product env var, or product policy in the code. A product sends what it needs as `Options.Env`, `StartSpec.Env`, and labels.
- Labels are opaque to the visor. It stores them, filters on them, and returns them. It never reads their meaning. The `visor.*` label namespace is reserved for the visor.
- Same machine only. The visor never talks to another machine. Remote access goes through the product.
- One visor for each product. Each has its own home, socket, `bin/`, and logins. Do not share state between two visors.

## Repository structure

The layout below is the plan. Make a package only when its first code lands.

```text
*.go                 # package agentvisor: Visor, Client, Serve, Dial, Ensure, specs, Session, Event,
                     # Backend, the built-in wire and pty backends, the socket routes
backend/orca/        # optional backend; a product registers it in Options.Backends
harness/             # detect, install, update, login, auth mode, models (uses agent-wire recipes)
workdir/             # projects from paths, worktrees, setup and teardown scripts, ports
holder/              # the holder process for PTY and wire sessions (leaf)
internal/store/      # visor.db: SQLite open, migrations, rows
cli/                 # the exported command tree; a product mounts it under its own binary
cmd/agent-visor/     # thin main for the generic CLI
contract/            # socket protocol: JSON schema and fixtures
```

Dependency direction is one way:

```text
cmd/agent-visor ──▶ cli ──▶ agentvisor ──▶ harness, workdir, holder, internal/store ──▶ agent-wire
backend/orca ──▶ agentvisor
```

- The root package imports `harness`, `workdir`, `holder`, and `internal/store`. Those packages never import the root package. This keeps the graph free of cycles.
- `holder` is a leaf. It imports nothing from this repository, so an old holder binary keeps its protocol.
- `backend/orca` imports the root. The root never imports a backend package.
- `cli` uses only the exported API of the root package. A product mounts the same tree under its own binary.
- Only the root package, `backend/*`, and `cli` are part of the public API. Put other code under `internal/` when no product needs it.

## Public API

This module is public, and products pin it. Treat an exported identifier as a promise.

- `*Visor` runs in the process. `*Client` talks to a visor in another process. Both have the same method set. Do not add a method to one and not the other.
- Return concrete types. Each product defines the small interface that it needs.
- Specs are structs with an empty value that works (`StartSpec`, `WorkdirSpec`, `Filter`). Add a field, do not add a positional argument.
- Statuses, kinds, roles, event types, and caps are closed enums declared once. Do not spell an enum value inline.
- `Caps` carries the difference between backends. A backend with fewer capabilities must not shape the interface.
- Before v1.0.0, a breaking change is allowed. Write it in `CHANGELOG.md`.

## Socket protocol

HTTP over a Unix socket with mode 0600. The event stream and attach use WebSocket. There is no TCP listener.

- The contract lives in `contract/`: a JSON schema and fixtures for the routes and the frames. The Go types are the source for Go clients. Non-Go clients vendor the fixtures byte-identical.
- Change the fixtures and the Go types in the same commit. A test checks that the Go types encode to the fixtures.
- A break bumps the protocol version that `Info` returns. The client refuses a visor with a protocol that it does not know.
- The holder protocol is separate and small. An old holder runs until its session ends, so the visor must speak each holder protocol version that can still run.

## Sessions, holders, and the event log

- The event sequence is global for one visor. A replay after a sequence number has no gaps and no duplicates. A test checks this for each backend.
- A holder owns the PTY or the stdio pipes of the vendor process. The visor never owns them directly, so a session survives a visor upgrade. After boot, the visor finds the holders with `Backend.Recover`.
- The wire holder exits when its session becomes idle. The next send starts a holder of the current build.
- Each goroutine has a stop condition (cancel, `WaitGroup`, or `errgroup`). The starter owns its lifetime. `Close` stops all of them.

## Env and secrets

Env comes in three layers. The last layer wins: visor env (`Options.Env`), harness env, session env (`StartSpec.Env`).

- Env values never go into events, logs, errors, or API responses. The API returns only the env keys.
- The visor keeps the start spec with env in `sessions/<id>/spec.json` with mode 0600, so it can resume a session with no call to the product.
- Each file that holds env, credentials, or raw frames has mode 0600. Each directory has mode 0700.
- `Options.Home` owns every path. Do not build a path from `os.UserHomeDir()` anywhere else.

## Types, methods, functions

- If several functions share the same first argument of type `T` / `*T`, they belong on `T` as methods.
- Put related state on a struct. Inject dependencies in `NewT(...)`.
- Unexported fields by default. Export a field only when it is part of the API and needs no invariant.
- Consistent receivers per type: all pointer or all value. Use a pointer if the method mutates, contains a mutex, or the struct is not tiny.
- Package-level functions are OK for `NewT` / `MustT` constructors, pure conversions that do not belong to one type, and orchestration across two or more types.
- Do not extract a one-off function unless it is reused or it names a real concept. Duplicate, then extract after the second use.

```go
// avoid
func ValidateSpec(s *StartSpec) error { ... }
func SaveSession(db *sql.DB, s *Session) error { ... }

// prefer
type sessionStore struct{ db *sql.DB }

func newSessionStore(db *sql.DB) *sessionStore { return &sessionStore{db: db} }

func (s *sessionStore) Save(ctx context.Context, sess Session) error { ... }
func (s StartSpec) Validate() error { ... }
```

## Files

One file per type or per topic, not one file per function.

- A new function goes in the existing file for its type or topic.
- Make a new file only for a new type, a new backend, or a second named concern.
- Forbidden: `helpers.go`, `utils.go`, `misc.go`, and files named after one function.
- Socket routes: the server lives in `server.go`, and the handlers are in `routes_<area>.go`, with the route table at the top of each file. The client methods for an area go in `client_<area>.go` when `client.go` gets too large.
- One file per backend: `backend_wire.go`, `backend_pty.go`.
- Tests: `foo_test.go` next to `foo.go`.
- Limit function size (`funlen`, `gocyclo`), not file size. A soft cap of 800 to 1200 lines is a prompt to find a seam. If you cannot name the seam, keep the file.

## Interfaces

- Accept interfaces, return concrete structs.
- `Backend` and `Handle` are the documented exceptions: the visor defines them because a product can supply a backend.
- Keep interfaces small and defined by the consumer. Do not define an interface for mocking.
- Check implementations at compile time when useful: `var _ Backend = (*ptyBackend)(nil)`.

## Constructors, wiring, state

- No package-level mutable state. Allowed package-level vars: sentinel errors, `regexp.MustCompile`, `//go:embed` contents, and `version`. Anything else needs `//nolint:gochecknoglobals` with a reason on the same line.
- No `init()`.
- Read env vars once, in `cmd/agent-visor` or in the product. Do not read them deep in the call stack.
- `NewT` returns `*T` or `(*T, error)`. `MustT` panics; use it only in `main` or tests.

## Context, errors, concurrency

- `context.Context` is the first argument of each function that does I/O, waits, or calls something that does.
- Do not store a request `context.Context` on a struct. A process-lifetime context that `Close` cancels is the exception.
- Wrap errors with context: `fmt.Errorf("start session %s: %w", id, err)`.
- Sentinel errors at package level. Compare with `errors.Is` / `errors.As`. The client maps a socket error code back to the same sentinel.
- Do not panic in library code, except in `Must*` constructors.
- Do not ignore an error with `_ =` unless the reason is on that line.

## API and naming

- MixedCaps. Do not stutter: `agentvisor.Session`, not `agentvisor.VisorSession`.
- Say "machine", not "computer" or "host", in names, docs, and errors.
- An exported identifier needs a comment only when its name does not say what it is.

## Logging

The library takes `Options.Logger`. When it is nil, use `slog.Default()`. Each subservice sets `log` in `NewT` with `.With("source", "<name>")`, one snake_case word. Instance ids go on a child logger: `session_id`, `workdir_id`, `harness`.

- Libraries return errors. Log at the edge: a route handler, a backend loop, a holder.
- Never log an env value, a credential, or the content of a turn.
- No `log.Printf`, no package-level logger, no `fmt.Println` for ops.

## Tests

- Table-driven tests next to the code that they cover. Use `t.Parallel()` when the test has no shared mutable state.
- The agent-wire fake harness drives each backend. Do not call a real vendor CLI in a unit test.
- Contract tests: the client tests run against a `Visor` through `Serve` on a temporary socket.
- Restart tests: kill the client during a turn, kill the visor with SIGKILL, and do a quiet upgrade. Check the event log after each.
- Live tests use the build tag `live` and real vendor CLIs. CI does not run them.
- Use `t.TempDir()` for `Options.Home`. Keep socket paths short, because macOS limits a Unix socket path to 104 bytes.
- Run `go test -race ./...`.

## Lint and format

New code must pass:

```bash
GOWORK=off golangci-lint run ./...
GOWORK=off go test -race ./...
```

See `.golangci.yaml`. It has the standard rules plus `bodyclose`, `errorlint`, and `noctx`. CI runs the tests on the Linux and macOS runners and the linter on Linux, on each push and pull request.

- Format with `gofmt` and `goimports`. Do not hand-format imports.
- Do not add `//nolint` without a reason on the same line.
- Set `GOWORK=off` when a `go.work` file in a parent folder does not list this module.

## What not to do

- Do not put product words or product policy in this repository.
- Do not add vendor knowledge here. Put it in agent-wire.
- Do not add a TCP listener or a path to another machine.
- Do not log or return env values.
- Do not add a package-level helper for logic that is used once and belongs on an existing type.
- Do not add getters and setters with no invariant.
- Do not add `handlers/`, `models/`, or `services/` folders that only sort file types.
- Do not add a global client, DB handle, or logger.

When unsure: put the new code on the existing type, in the existing file, with dependencies passed into `NewT`. Split only when you can name the second concern in a few words.
