# The MCP server

The API binary serves a Model Context Protocol server at `POST /mcp`. An
assistant that speaks MCP (Claude, ChatGPT, Cursor, a custom agent) connects
to it, the user signs in to ReelPin once through the browser, and from then on
the assistant can read the user's library, search it, save links and file
reels into collections. It replaces the in-app chat.

The tools call the same `Service` types the REST handlers call, so a reel
looks the same whichever door it came through. Nothing is served from a
separate process: `cmd/api` is the whole deployment.

## How a client connects

The server implements the MCP specification revision `2026-07-28`
([spec](https://modelcontextprotocol.io/specification/2026-07-28)) over the
stateless Streamable HTTP binding
([transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)),
through the official Go SDK `github.com/modelcontextprotocol/go-sdk` v1.8.0.

| Environment | MCP endpoint | Protected-resource metadata |
|-------------|--------------|-----------------------------|
| Production | `https://api.reelpin.in/mcp` | `https://api.reelpin.in/.well-known/oauth-protected-resource/mcp` |
| Development | `https://api-dev.reelpin.in/mcp` | `https://api-dev.reelpin.in/.well-known/oauth-protected-resource/mcp` |

Both hosts come from `PUBLIC_API_BASE_URL`. Empty means `/mcp` is not served
at all, because a protected resource that cannot name itself cannot publish
valid metadata.

What the transport does and does not do:

- Every request is an HTTP `POST` to `/mcp` carrying one JSON-RPC message,
  with `MCP-Protocol-Version: 2026-07-28` and the per-request `_meta` fields
  the revision requires. Answers are `application/json`. The one exception is
  `subscriptions/listen`, which the SDK always answers as an SSE stream; it
  stays open until the client disconnects or the server's 15 s write timeout
  ends it, and no tool list ever changes while it is open.
- There are no sessions. `GET` and `DELETE` answer `405`, an `Mcp-Session-Id`
  header is ignored, and nothing is resumable. A process behind nginx needs
  no sticky routing.
- A client on revision `2025-11-25` or earlier still connects: the SDK answers
  its `initialize` handshake and then treats every request on its own.
- Bodies are bounded at 1 MiB, the same as every v2 JSON body.
- A browser request from another site, judged by `Sec-Fetch-Site` or by
  `Origin` against `Host`, answers `403 cross_origin_forbidden` before any
  token is read, as the transport spec requires. Assistants and desktop
  clients send neither header and are unaffected.
- `/mcp` goes through the same request-id, log and metrics middleware as
  `/api/v2`. The metrics label is the mux pattern `/mcp`; log lines carry the
  hashed user, never the id, the token or a tool argument.

## The OAuth flow

MCP authorization
([spec](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization))
makes this server an OAuth 2.1 resource server and the Supabase project's
OAuth 2.1 server the authorization server. The same Supabase project signs the
app's session tokens, so one identity opens one library on the phone, the
website and an assistant. See
[decision 0005](decisions/0005-share-supabase-auth-across-clients.md).

The steps a client takes, each with the document or endpoint it uses:

1. `POST /mcp` with no token answers `401` with
   `WWW-Authenticate: Bearer resource_metadata="https://api.reelpin.in/.well-known/oauth-protected-resource/mcp"`
   ([RFC 9728 §5.1](https://datatracker.ietf.org/doc/html/rfc9728#section-5.1)).
   A rejected token answers the same with `error="invalid_token"`.
2. The client fetches that document. It names the resource
   (`https://api.reelpin.in/mcp`), the authorization server
   (`https://rdzykswefpgeolbamend.supabase.co/auth/v1`, which is
   `SUPABASE_URL` plus `/auth/v1`, the issuer every token is verified against)
   and `bearer_methods_supported: ["header"]`. It answers `GET` and `HEAD`.
   There is deliberately no document at the root path
   `/.well-known/oauth-protected-resource`: RFC 9728 §3.3 has that one name
   the origin, which is not a protected resource here, and a client that
   read `/mcp` from it would have to reject it.
3. The client fetches the authorization server's metadata
   ([RFC 8414](https://datatracker.ietf.org/doc/html/rfc8414)). For an issuer
   with a path the spec tries the path-inserted form first, which is the one
   Supabase documents:
   `https://rdzykswefpgeolbamend.supabase.co/.well-known/oauth-authorization-server/auth/v1`.
   The OpenID Connect form,
   `https://rdzykswefpgeolbamend.supabase.co/auth/v1/.well-known/openid-configuration`,
   also works. The document names the endpoints below and advertises
   `code_challenge_methods_supported`, which a client must see before it
   proceeds.
4. The client obtains a `client_id`. Supabase supports two of the spec's three
   ways: a client pre-registered in the dashboard (**Authentication → OAuth
   Apps → Add a new client**: a name, one or more redirect URIs, public or
   confidential), or dynamic client registration
   ([RFC 7591](https://datatracker.ietf.org/doc/html/rfc7591)) once it is
   switched on under **Authentication → OAuth Server**. Dynamic registration
   is what lets a desktop assistant connect without anyone registering it
   first; the spec marks it deprecated in favour of Client ID Metadata
   Documents, which Supabase does not offer yet, and keeps it for exactly this
   case. The registration endpoint is the `registration_endpoint` in the
   metadata document; the spec tells clients to use `application_type:
   "native"` for a desktop or CLI client.
5. Authorization code with PKCE `S256` at
   `https://rdzykswefpgeolbamend.supabase.co/auth/v1/oauth/authorize`
   (`response_type=code`, `client_id`, `redirect_uri`, `code_challenge`,
   `code_challenge_method=S256`, `state`; the client also sends
   `resource=https://api.reelpin.in/mcp` as the spec requires, which Supabase
   ignores). Supabase sends the user to the website's consent page, the
   **Authorization Path** configured under **Authentication → OAuth Server**
   and appended to the project's Site URL, with an `authorization_id`. That
   page signs the user in if needed and approves or denies through
   `supabase.auth.oauth.getAuthorizationDetails`, `approveAuthorization` and
   `denyAuthorization`. The page lives in the web repo; it is the one piece of
   this flow that is not in this repository.
6. The client exchanges the code at
   `https://rdzykswefpgeolbamend.supabase.co/auth/v1/oauth/token`
   (`grant_type=authorization_code`, `code`, `redirect_uri`, `code_verifier`,
   `client_id`), and later refreshes with `grant_type=refresh_token`. Access
   tokens live one hour by default; refresh tokens rotate.
7. Every MCP request carries `Authorization: Bearer <access token>`.

Supabase's documentation for each piece: the
[OAuth 2.1 server overview](https://supabase.com/docs/guides/auth/oauth-server),
[getting started](https://supabase.com/docs/guides/auth/oauth-server/getting-started)
(enabling it, registering a client, the consent page),
[the flows](https://supabase.com/docs/guides/auth/oauth-server/oauth-flows),
[MCP authentication](https://supabase.com/docs/guides/auth/oauth-server/mcp-authentication)
and [token security](https://supabase.com/docs/guides/auth/oauth-server/token-security).
The server is available on every plan including Free, is in beta, and costs
nothing beyond the monthly active users it signs in.

### What the token is and how it is checked

An access token from the OAuth server is an ordinary Supabase JWT: `iss` is
`https://rdzykswefpgeolbamend.supabase.co/auth/v1`, `aud` is `authenticated`,
`role` is `authenticated`, `sub` is the user, and `client_id` is the OAuth
client it was minted for. It is signed with the project's current signing key
and published through `/auth/v1/.well-known/jwks.json`. The project must use
an asymmetric (ES256) signing key, which it already does for the app: the
verifier accepts nothing else, and Supabase itself refuses the `openid` scope
under HS256.

`/mcp` verifies it with the same `internal/auth` verifier as `/api/v2`:
signature, issuer, audience, expiry, role and a UUID subject. It then
requires one more thing: a `client_id`. A session token the app or the website
holds has none and is refused. Only a token the OAuth server minted for a
registered client gets in, which is what the authorization spec means by a
token issued for this server, and it is what keys the per-client rate limits.

The spec also asks clients to bind a token to the resource with
[RFC 8707](https://www.rfc-editor.org/rfc/rfc8707.html) and asks servers to
check that binding. Supabase does not implement resource indicators: it
ignores the `resource` parameter and always sets `aud` to `authenticated`.
What binds a token here instead is the issuer and signing key of this one
project, whose only resource server is this API, plus the `client_id` above. A
Custom Access Token Hook could write the MCP resource into `aud`, but the
verifier checks `aud` against `SUPABASE_JWT_AUDIENCE` for every route, so that
is a change for `/api/v2` and `/mcp` together or for neither.

### Scopes

Supabase's scopes (`openid`, `email`, `profile`, `phone`) govern which OpenID
claims a client may read, not what a token may do with this API, and custom
scopes are not supported. The server therefore advertises no
`scopes_supported` and no `scope` in its challenge; a client that sends none
gets Supabase's default, `email`. A token grants the whole of the user's own
library, exactly as the app's session does.

## The tools

Every tool acts on the signed-in user's own library. Ids are the same UUIDs
the REST API uses. Every reel carries `web_url`, the public page on
`PUBLIC_WEB_BASE_URL`, which is the link to hand the user. A tool that cannot
do what was asked answers a tool error with a sentence the assistant can act
on; a driver error never reaches it.

| Tool | Input | Answers |
|------|-------|---------|
| `list_reels` | `platform`, `category`, `subcategory`, `saved_date` (`YYYY-MM-DD`), `cursor`, `limit` (1–100, default 25), all optional | `reels` (compact cards), `next_cursor`, `has_more` |
| `get_reel` | `reel_id` | The reel in full: summary, transcript, key facts, tags, people, action items, events, places with coordinates and a Google Maps link |
| `search_reels` | `query`; the same filters as `list_reels`; `limit` (1–20, default 5) | `results` of `reel`, `relevance_percent`, `match_label`; `search_mode` names the arms that ran |
| `save_link` | `url`; optional `collection_ids`, `idempotency_key` (a UUID) | `status` (`saved`, `processing`, or a replayed job's final `completed`, `failed`, `dead_lettered`), `already_saved`, and the `reel` or the `job` |
| `list_collections` | none | `collections`: owned and shared, each with `role`, `item_count`, `web_url` |
| `get_collection` | `collection_id`; optional `cursor`, `limit` | The `collection`, a page of `items`, `can_edit` |
| `add_to_collection` | `collection_id`, `reel_ids` (up to 100) | The `collection` as it now stands and `added_count` |

A compact card is `id`, `title`, `summary`, `platform`, `category`,
`sub_category`, `url`, `web_url`, `thumbnail_url`, `saved_at` and `location`.
The transcript and the long-form extraction are only on `get_reel`, the same
rule `docs/api-contract.md` gives for the REST list and search.

`save_link` has the semantics of `POST /api/v2/processing-jobs/reels`: a link
the user already has answers `already_saved: true` with the reel, or with the
job still processing it; a new link starts a job. `idempotency_key` is the
same attempt key the REST route takes in its header, scoped to this tool, and
is minted by the server when the client sends none. A retry with the same key
within 24 hours answers the same job as it stands now, so its `status` can
be a final one. Processing takes up to a
few minutes; the assistant polls nothing, it tells the user and the reel
appears in `list_reels` and `search_reels` once done.

There are no resources and no prompts. A reel as a resource template would be
a second way to spell `get_reel`, and nothing in the product needs a prompt.

## Limits

The costed tools share the REST routes' buckets from
`internal/ratelimit/policies.go`, and every bucket must allow a call:

| Tool | Per OAuth client, under the user | Per user | Per IP |
|------|----------------------------------|----------|--------|
| `search_reels` | 30 per minute | 30 per minute | 90 per minute |
| `save_link` | 30 per hour | 30 per hour | 60 per hour |

A second client registered by the same user does not add to their allowance.
Five submissions may be processing at once. Without Redis there is no limiter,
and `search_reels` and `save_link` answer that they are unavailable rather
than spend unmetered, while the reads keep working: the same fail-closed rule
as the REST routes.

A refused call answers a tool error naming how many seconds to wait. An
invalid or expired token answers `401` with the challenge above, which a
client treats as the signal to refresh or re-authorize.

## What the owner sets up once

- In the Supabase dashboard: switch on the OAuth 2.1 server, set the
  Authorization Path to the website's consent page, and either register each
  assistant as an OAuth client or enable dynamic client registration.
- In the web repo: the consent page at that path.
- On each API host: `PUBLIC_API_BASE_URL`.

## Trying it by hand

With the API running locally and `PUBLIC_API_BASE_URL=http://localhost:8000`:

```sh
curl -s http://localhost:8000/.well-known/oauth-protected-resource/mcp
```

```sh
curl -s -i http://localhost:8000/mcp -X POST \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: tools/list' \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
```

`$TOKEN` is an access token from the OAuth flow above; a session token from
the app answers `401`. The integration test in
`internal/httpapi/mcp_integration_test.go` drives the server with the SDK's
own client against a database and is the fastest way to see every tool run.
