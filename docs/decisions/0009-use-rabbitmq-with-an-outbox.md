# 9. Use RabbitMQ with a transactional outbox

**Status:** accepted

## Context

Processing may take minutes and call several providers. An API response cannot
hold that work, and a database commit followed by a direct broker publish can
lose the job if the process stops between those operations.

## Decision

Use RabbitMQ durable classic queues on the initial single node. Publish
persistent messages with confirms, consume with manual acknowledgements and use
bounded retry queues plus a dead-letter queue.

Create the processing state and an outbox event in one PostgreSQL transaction.
An outbox publisher sends committed events. Consumers acknowledge only after a
durable checkpoint. Delivery is at least once, so every stage and final effect
is idempotent.

## Consequences

A process or broker restart may deliver the same message again, but it cannot
lose committed work or duplicate its effect. PostgreSQL run and outbox state can
rebuild unfinished broker work.

This adds an outbox publisher, leases and replay tooling. It does not claim
exactly-once delivery across PostgreSQL and RabbitMQ.
