# Changelog

## Unreleased

- Add the library core: `Visor`, `StartSpec`, `Session`, `Event`, `Filter`, and the `Backend` and `Handle` interfaces.
- Add the built-in `wire` backend over agent-wire.
- Add the event log in SQLite with a global sequence. A replay after a sequence has no gaps and no duplicates.
- Add the session states, the restart policy, suspend at close, and lost at boot.
- Add the `workdir` package: projects from paths, the main checkout, git worktrees by key, plain folders, setup and teardown scripts, and port ranges.
- Keep the start spec with env in `sessions/<id>/spec.json` with mode 0600. The API returns only the env keys.
- Add the socket server (`Serve`, `Listen`, `NewServer`) and `Client` (`Dial`). `Client` has the same method set as `Visor`, and its event stream connects again after a drop and continues after the last seq.
- Add `Hello` and `ErrProtocol`. `Dial` refuses a visor with another protocol major.
- Add the `contract/` folder: the routes, the JSON schema, and fixtures, checked by `TestContract`.
- Add the `service` package: install the visor as a LaunchAgent or a systemd user unit, and `Ensure`.
- Add JSON types for permissions, MCP servers, skills, and attachments in `StartSpec` and `Input`.
- Add the `harness` package and the `Visor` harness methods: `Harnesses`, `Harness`, `InstallHarness`, `UpdateHarness`, `SetHarnessAuth`, `LoginHarness`, `LogoutHarness`, `HarnessModels`, and `SetHarnessEnv`.
- Install a visor copy of a harness from the agent-wire recipe in `bin/<harness>/<version>/` and move the `current` link. A failed install keeps the current version. An install or an update gives `ErrBusy` while a live session uses the harness.
- Add the auth modes `shared` (the user login) and `separate` (`harness/<harness>/` in the visor home), and a login in a pipe that gives the URL or the device code.
- Keep the harness env in `harness/<harness>.env.json` with mode 0600. A session gets the env layers visor, harness, and session. The API returns only the env keys.
- Set `Launch.Binary`, `Launch.Home`, and the harness env when a session starts. Emit `harness.changed` on each harness change.
- Move `Limits` and `LimitWindow` to the `harness` package. The root types are aliases.
- Add the `cli` package: `Command(Options)` gives the command tree, so a product mounts it under its own binary with its own home. It talks to the visor through the `cli.Visor` interface, and `Options.Dial` and `Options.Configure` let a product or a test replace the connection and the `serve` options.
- Add the `agent-visor` binary with `serve`, `status`, `version`, `ls`, `project`, `workdir`, `session`, and `harness`, and the global flags `--home`, `--socket`, and `--json`. `upgrade`, `hook`, `hold`, `session shell`, `session attach`, and `session handover` say "not available yet". The `harness` commands work on a visor connection that has the harness methods; they say "not available yet" on a `Client` without them.
