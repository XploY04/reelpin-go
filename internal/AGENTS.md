# internal/

The service. See [`../ARCHITECTURE.md`](../ARCHITECTURE.md) for how these fit
together and [`../docs/decisions/0001-layered-packages.md`](../docs/decisions/0001-layered-packages.md)
for why.

## The layering rule

Dependencies point inward, and `make check` fails on a violation:

- `reels` and `jobs` are the domain. They import no transport package, no
  driver, and not each other's storage. They declare what they need from storage
  as an interface.
- `postgres` implements those interfaces. **It is the only package with SQL in
  it.**
- `httpapi` depends on the interfaces, never on `postgres`.
- `config` imports nothing from `internal/`. It is read once, at startup.

An interface is declared in the package that **consumes** it, not the one that
implements it. `ReelReader` lives in `reels` because `reels` is what needs
reading; `postgres` just satisfies it.

## Rules that differ here

- **A package name is a noun that means something in this domain**, not a layer
  label. `reels`, not `models`. `postgres`, not `repository`.
- **Errors cross package boundaries as sentinels or typed errors**, never as
  strings to be matched. `ErrNotFound` is checked with `errors.Is`.
- **A new dependency needs a reason in the pull request.** Most of this is
  standard library on purpose, and the exceptions (pgx, jwx) are there because
  writing them by hand would be worse.
- **Tests do not need a database unless they are testing SQL.** Anything under a
  `//go:build integration` tag may use one; everything else uses a fake and runs
  in milliseconds.
