# Socket contract

This folder is the contract of the socket protocol `agent-visor/1`: HTTP over a Unix socket, with WebSocket for the event stream and attach.

- `routes.json` lists the routes.
- `schema.json` is the JSON schema of each message. The Go types make it.
- `fixtures/` holds one example of each message.

The Go types are the source. `TestContract` checks that the Go types encode to these files. After a change to a type, run `go test -run TestContract -update .` and commit the files with the change.

A client in another language vendors these files byte-identical. A break bumps the protocol version that `GET /v1/info` returns.

Errors have the body `{"error": {"code": "...", "message": "..."}}`. The codes are `not_found`, `invalid`, `state`, `busy`, `closed`, `unsupported`, `protocol`, and `internal`.

The event stream `GET /v1/events?after=<seq>` sends one JSON event in each text frame: first the events after `after` from the log, then the live events, with no gaps and no duplicates. Close code 4001 means that the visor stops; 4002 means that the stream failed. A client connects again with the last seq that it got.
