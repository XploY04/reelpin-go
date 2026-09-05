# 13. Start the Go client contract at API v2

**Status:** accepted

## Context

Installed Flutter clients already call the Python `/api/v1` contract. The Go
contract changes cursors, errors, job states and some request bodies. Mobile
clients do not all update at the same time.

## Decision

Go owns `/api/v2`. ReelPin web and updated Flutter builds use v2. Python keeps
serving `/api/v1` during the measured mobile support window.

Support the current and previous two Flutter releases. Keep v1 for at least 90
days after the v2 release and until v1 traffic is below 1 percent of active
devices for 30 consecutive days, whichever is later.

## Consequences

The Go contract can be internally consistent without breaking installed apps.
Both API versions and legacy synchronization exist during migration.

Future additive changes stay in v2. A future breaking contract uses v3.
