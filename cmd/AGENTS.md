# cmd/

Binaries. Each `main` package here does one thing: build the real dependencies
and hand them to something in `internal/`.

## Rules that differ here

- **No logic in `main`.** If a function here does something worth testing, it is
  in the wrong package. `cmd/api/main.go` reads config, opens a pool, fetches
  the JWKS, builds the server, listens, and drains. That is the whole job.
- **Startup failures are fatal and loud.** A missing required variable or an
  unreachable dependency exits non-zero with a structured log line. Never start
  degraded and answer 500s: a process that refuses to start is diagnosable, one
  that starts broken is not.
- **Shutdown order is deliberate.** Stop serving, drain for 10s, then close the
  dependencies in the reverse order they were opened. Closing a pool while a
  request still holds a connection is a panic in production and nothing in a
  test.
- **This is the only place that knows the concrete types.** `cmd/api` imports
  `internal/postgres` because something has to; nothing under `internal/httpapi`
  ever does.

## Adding a binary

A new binary is a new directory with its own `main.go` and its own wiring. It
does not share a `main` with another, and it does not grow a subcommand flag to
become two programs.
