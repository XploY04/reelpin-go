# 8. Separate global content from user saves

**Status:** accepted

## Context

Two users may save the same public URL. Downloading, transcribing and extracting
it twice wastes time and provider cost, but one user's library must never become
visible to another user.

## Decision

Store a public source once in `contents`, retain immutable extraction results in
`content_versions`, and represent ownership with `user_saves`. One public
content identity may have many private saves. `UNIQUE (user_id, content_id)`
prevents duplicate saves for one user.

The public API exposes `user_saves.id` as the reel ID and keeps `contents.id`
internal. Private or credential-scoped sources include a user-specific access
scope and do not deduplicate across users.

## Consequences

Public work is processed once and reused. Every read and mutation still starts
from the authenticated user's save, so sharing processing does not share
ownership.

Deleting a reel removes that user's save. Public global content remains reusable
unless privacy, legal, private-source or blocklist policy requires a full purge.
