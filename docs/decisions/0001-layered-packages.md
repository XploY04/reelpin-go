# 1. The domain does not know about transport or drivers

**Status:** accepted

## Context

The Python service this replaces keeps request handling, business rules and
database access in one module. It works, and it is the reason a change to a
query means reading a route handler, and a change to a response shape means
reading SQL. Tests need a database because there is no seam without one.

Go makes the alternative cheap: an interface declared where it is consumed
costs nothing at runtime and nothing in ceremony.

## Decision

Dependencies point inward, and the domain is the middle:

- `internal/reels` and `internal/jobs` hold the types and the rules, and declare
  what they need from storage as interfaces (`ReelReader`, `JobReader`). They
  import no transport package and no driver.
- `internal/postgres` implements those interfaces. It is the only package that
  contains SQL.
- `internal/httpapi` depends on the interfaces, never on `internal/postgres`.
- `cmd/api` is the only package that knows all of them exist, and only wires
  them together.

`make check` fails on a violation, so this is enforced rather than remembered.

## Consequences

Handler tests run with a fake reader and no database, which is why the unit
suite needs no services and finishes in seconds. Swapping storage means writing
one package. Reading how a response is shaped means reading one package.

The cost is one indirection: finding "the code that runs" for an endpoint means
following an interface to its implementation. That is a real cost and it is
worth it here, because the alternative is the thing being replaced.

The rule only pays while it is total. One handler reaching for `pgx` because it
is quicker removes the property for everything, which is why it is a CI failure
and not a review comment.
