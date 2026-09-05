# 4. Isolate environments with infrastructure

**Status:** accepted

## Context

Development code must not read production users, claim production jobs or share
provider cooldowns. An `environment` column can filter rows, but every query,
unique index and background claim has to remember it. One missed predicate
breaks the boundary.

## Decision

Development and production use separate Supabase projects, Redis instances and
RabbitMQ virtual hosts. Keep `ENVIRONMENT` as process configuration for logs,
validation and deployment behavior. Do not store it as a product-data field.

The current Supabase project is production. The `reelpin-go` project is
development.

## Consequences

A development process cannot reach production data with its configured
credentials. Database uniqueness and queue claims are simpler because they do
not repeat an environment predicate.

The cost is another set of resources, migrations and secrets. Repository
commands must be able to create an empty development environment, and production
migrations must still preserve existing data.

The current EC2 development host and the provided production EC2 are in Mumbai,
while both Supabase projects are in Tokyo. A development readiness ping took
about 265 ms. The product owner chose the existing Mumbai production host, so
the production rehearsal must measure and explicitly accept database latency;
an SLO failure is the trigger to move compute near the database.
