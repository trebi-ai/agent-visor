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
