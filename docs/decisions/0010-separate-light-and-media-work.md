# 10. Separate light and media work

**Status:** accepted

## Context

The Python worker has one shared queue. Two long media jobs can occupy both
worker threads and make a short HTML save wait. The development host cannot
safely run many platform-specific processes.

## Decision

Run one Go worker process with separate media, light and notification consumers.
A topic exchange routes work by deterministic source metadata. Platform handlers
are registered inside the process; adding a platform does not add an idle OS
process.

Development runs one media and one light consumer. Production starts with the
same limits and may add a second media consumer only after host load and memory
tests pass. Unknown web URLs start with bounded light inspection and move to
media through a committed outbox event when required.

## Consequences

Short work keeps reserved capacity while expensive media stays bounded. One
binary and one deployment remain enough for the initial scale.

Routing does not need an AI call. Capacity can later grow by changing consumer
counts or running another copy of the same worker binary.
