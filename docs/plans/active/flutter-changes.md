# Flutter changes for the Go v2 API

A running record for the app developer of everything the Flutter app and both
native share extensions must change to move from the Python `/api/v1` service to
the Go `/api/v2` service. Updated in the same pull request as each backend
change, so it is never behind the contract.

The machine-readable source of truth is `api/openapi.yaml` (embedded and
published with a SHA-256 digest per release). This page is the narrative: what
changed, why, and what it means in the app. Task 30 of
`implementation-plan.md` is the migration task itself; nothing here requires an
app release before then, because Python v1 keeps serving installed versions
throughout coexistence.

## Base URL and versioning

- Dev: `https://api-dev.reelpin.in/api/v2/...` (once Task 22 deploys it).
- Production: `/api/v1` stays Python, `/api/v2` is Go. Updated clients call v2
  only; route selection is done by path, never by anything the client sets.
- **Bare aliases are gone.** The shipped app calls some endpoints without the
  `/api/v1` prefix (`/reels`, `/processing-jobs`, ...). v2 has no bare aliases;
  every call must use the full `/api/v2/<path>`.

## Error envelope (every endpoint)

v1 errors were flat and inconsistent. Every v2 error, including 404 and 405, is:

```json
{
  "error": {
    "code": "reel_not_found",
    "message": "The requested record was not found.",
    "request_id": "abc123...",
    "retryable": false,
    "details": {}
  }
}
```

- Match on `error.code`, never on `message` (message text may change).
- `success`, `error_code`, `detail` and top-level `allowed` no longer exist.
  The allowed-platform list now arrives as `error.details.allowed`.
- Show or log `request_id` in support flows; it is the correlation handle.
- `retryable` says whether repeating the same request may succeed.

## Pagination: opaque cursors replace offsets

`GET /api/v2/reels`:

- **Request**: `cursor` (opaque string from the previous page), `limit`
  (default 25, max 100 — v1 defaulted to 50). `offset` and `sort` are gone;
  there is one order, newest saved first.
- **Response**: `{"reels": [...], "next_cursor": "...", "has_more": bool,
  "limit": n}`. `total_count`, `offset` and `next_offset` are gone.
- Treat `next_cursor` as a black box. Do not parse it, store it across app
  launches, or synthesize one. Sending anything the API did not issue is a
  `422 validation_error`.
- The old client behaviour of passing an integer cursor/offset will be rejected,
  not silently accepted.

## Submission (built)

`POST /api/v2/processing-jobs/reels`, bearer auth:

- Body: `{"url": "...", "collection_ids": ["..."]}`.
- **New required header: `Idempotency-Key: <uuid>`.** Generate one UUID per
  submission attempt in the UI; reuse it for network retries of that attempt;
  new deliberate submission = new key. Reusing a key with a different body is a
  `409`.
- `200` = already saved, body is the reel itself (no job to poll).
- `202` = queued or already in flight, body is the processing job to poll.
- v1's single-status behaviour is gone; handle both.

**New error codes on this endpoint**, all matched on `error.code`:

| Code | Status | What the app should do |
|---|---|---|
| `active_job_limit` | 429 | Two submissions are already processing. Show "wait for one to finish"; do not retry automatically. |
| `idempotency_conflict` | 409 | The key was reused with a different body. This is a client bug: generate a new key. |
| `rate_limited` | 429 | Obey the `Retry-After` header; `error.details.retry_after_seconds` carries the same number. |
| `processing_unavailable` | 503 | Submissions are temporarily off (a safety dependency is down, or the cost gate has tripped). Retryable; reads still work. |
| `validation_error` | 422 | `error.details.field` names the offending field, including a missing `Idempotency-Key`. |

**The body is strict.** Unknown JSON fields are rejected with `422`, so a stale
client that still sends `user_id` in the body **will now fail**. Remove it
before pointing a build at v2.

## Native share extensions (Android + iOS) — biggest change

The two-call flow (resolve, then enqueue) is replaced by **one atomic call**,
because an extension can be killed between two calls:

- `POST /api/v2/native-shares/reels` with header `X-Share-Token: <token>` and
  body `{"raw_payload_text": "<exactly what the OS handed you>",
  "collection_ids": [...]}` plus `Idempotency-Key`.
- Responses are the same `200`-reel / `202`-job pair as above.
- A missing/invalid token is `401 share_token_required`.
- The extensions should persist the idempotency key with the pending payload so
  a retry after being killed cannot double-enqueue.

Bad or missing share tokens answer `401` with `share_token_required` (no header)
or `invalid_share_token` (unknown, expired or revoked — the three are
deliberately indistinguishable). On `invalid_share_token` the extension should
stop retrying and prompt the user to open the app, which mints a fresh token.

Share tokens are minted and revoked from the app process (bearer auth):

- `POST /api/v2/share-tokens` → `{"token": "...", "expires_at": "..."}`. The
  value is returned once; store it in the native-owned store as today.
- `DELETE /api/v2/share-tokens` revokes all of them (use on sign-out).

`POST /api/v2/share/resolve` (bearer) still exists, but only for interactive
in-app URL preview. The extensions must not call it any more.

## Endpoint-by-endpoint map

| v1 (Python) | v2 (Go) | Changes beyond the envelope |
|---|---|---|
| `GET /api/v1/health/live`, `/ready` | same paths under `/api/v2` | none |
| `GET /api/v1/health` | **removed** | poll `/ready` instead; it was a compat alias |
| `GET /api/v1/reels` (+ bare alias) | `GET /api/v2/reels` | cursor pagination, default limit 25, no `sort`, no `offset`, no `total_count` |
| `GET /api/v1/reels/{id}` | `GET /api/v2/reels/{reel_id}` | transcript still only here, never in lists |
| `GET /api/v1/reels/filters` | `GET /api/v2/reels/filters` | none |
| `GET /api/v1/reels/category-filters` | `GET /api/v2/reels/category-filters` | none |
| `GET /api/v1/processing-jobs` | `GET /api/v2/processing-jobs` | bounded list, no pagination; `active_only` default is still `false` |
| `GET /api/v1/processing-jobs/{id}` | `GET /api/v2/processing-jobs/{job_id}` | none |
| `POST /api/v1/processing-jobs/reels` | `POST /api/v2/processing-jobs/reels` | `Idempotency-Key` required; `200` vs `202` split |
| native share enqueue (v1 two-step) | `POST /api/v2/native-shares/reels` | one atomic call, `X-Share-Token` |
| `POST /api/v1/share-tokens` / `DELETE` | same paths under `/api/v2` | bearer only |
| `GET /api/v1/account/library-stats` | `GET /api/v2/account/library-stats` | none |
| `GET /api/v1/account/entitlements` | **not in v2 yet** | stays on v1 until a v2 decision; do not migrate this call |

## Collections (built)

15 operations under `/api/v2/collections`, all bearer except the shared view.
Item ids in requests and responses are `reel_id` and are unchanged save ids, so
existing local caches keep working. What changed beyond the envelope:

- **Detail shape**: `{collection, items, page:{next_cursor,has_more,limit},
  can_edit}`. `items` are **cards** (`reel_id, title, summary, url,
  thumbnail_url, source_platform, saved_at, added_at, added_by`), not full
  reels — fetch the full reel from `GET /api/v2/reels/{reel_id}`. The old
  `reels[]` plus `pagination{offset,total_count}` shape is gone.
- **Item paging is opaque cursors**, same rule as the library: pass
  `next_cursor` as `cursor`, never construct one; a v1-style offset is a `422`.
- **Shared view** moves to `GET /api/v2/shared-collections/{token}`, is
  unauthenticated, and no longer returns `owner_id`, `owner_name` or
  `member_count`.
- **Invite accept** moves to `POST /api/v2/collection-invites/{token}/accept`.
- **Links expire.** `enableCollectionLink` and `createCollectionInvite` return
  `expires_at` (links default to 30 days); the UI should surface that a link
  needs re-minting.
- **Member responses drop `display_name`** — there is no profiles table in the
  canonical schema yet. Show ids or resolve names client-side.
- New codes to match: `collection_not_found` (404), `collection_forbidden`
  (403), `collection_invite_invalid` (400).
- Request bodies reject unknown fields, so a misspelled key now fails loudly
  instead of being silently ignored.

`collection_ids` on submission stays rejected until enqueue and collections
share a trunk; this file will say when it is accepted.

## Notifications (built)

- `POST /api/v2/device-push-tokens` with `{"token": "...", "platform":
  "ios"|"android"|"web"}` — `platform` is now validated, so the old free-text
  value fails with `422`. Registering the same token again moves it to the
  current user, which is what makes a shared phone behave.
- `DELETE /api/v2/device-push-tokens` with the same body. Another user's token
  answers `404 device_token_not_found`.
- `POST /api/v2/notifications/{notification_id}/opened` — a second open, and
  another user's notification, both answer `404`.
- **The token is never echoed back** by any endpoint, so do not expect to read
  it from a response.
- **Campaign endpoints are gone from the product API.** Campaign sends are an
  operator command now; a client token can no longer reach them.
- One notification per job: the backend dedupes by job id, so a retried
  delivery cannot buzz the phone twice.

## Map (built)

- `GET /api/v2/map/pins?south=&west=&north=&east=` returns `{pins: [...]}`.
  A pin is `{id, kind, name, address, latitude, longitude, reel_id,
  confidence}`; `kind` is `content` (carries `reel_id`) or `manual` (does not).
- **A box whose `west` is greater than `east` is valid** and means a viewport
  crossing the antimeridian. Do not "normalise" it client-side; the backend
  handles it as two boxes and a normalised box would return the whole world.
- `GET /api/v2/map/nearby?latitude=&longitude=&radius_metres=&limit=` orders by
  real distance in metres.
- `POST /api/v2/map/manual-pins` returns `201` with the pin;
  `DELETE /api/v2/map/manual-pins/{pin_id}` returns `204`.
- `POST /api/v2/map/locations/{location_id}/hidden` with `{"hidden": true}`
  hides a pin **for this user only**. Hidden pins are already excluded from the
  responses above, so the client does not filter them.
- A bad or missing coordinate is `422` with `error.details.field` naming it,
  never a silent default that moves the map to the Gulf of Guinea.

## Sources and platforms (built)

`source_platform` on a reel or a job is one of `instagram`, `youtube`,
`tiktok`, `x`, `reddit`, `linkedin`, `pinterest`, or, for anything else, the
link's **hostname** (`google_maps`, `tripadvisor`, `zomato`, `someblog.com`).
Do not hard-code the list into a switch that throws on an unknown value: a new
handler adds a value without a client release. Treat an unrecognised platform
as generic and render the hostname.

`source_content_type` is `reel`, `post`, `video`, `short`, `page`, `pin`,
`profile` or `link`. Same rule: unknown means generic.

The app does no host filtering today and should keep doing none. Post the link
and let the backend decide; an unsupported link comes back as a `422` naming
`url`, with the allowed list in `error.details.allowed`.

## Share preview (built)

`POST /api/v2/share/resolve` with `{"raw_payload_text": "<whatever was
shared>"}` answers `200` in both cases:

```json
{"supported": true,  "url": "...", "source_platform": "instagram", "source_content_type": "reel"}
{"supported": false, "url": null,  "source_platform": null,        "source_content_type": null}
```

**Unsupported is a `200`, not an error.** It is a preview answer, so the share
sheet can say "we cannot read this link" without an error path. It also accepts
the messy text a share sheet actually hands over (a caption with a URL buried
in it), which is why it takes `raw_payload_text` rather than `url`.

This endpoint is bearer-authenticated and is for the in-app share sheet. The
native extensions do not need it: `POST /api/v2/native-shares/reels` resolves
the same payload itself in one call.

## Deletion (built)

`DELETE /api/v2/reels/{reel_id}` returns `204`. There is no body: the reel is
gone and the client already knows which one.

`DELETE /api/v2/account` returns `200` with:

```json
{
  "data_deleted": true,
  "identity_deleted": false,
  "pending": true,
  "removed": {"saves": 42, "processing_jobs": 3, "idempotency_keys": 12, "private_content": 7}
}
```

**`data_deleted: true` with `identity_deleted: false` and `pending: true` is a
real, expected outcome, not a failure.** It means the rows are gone but the
Supabase identity has not been removed yet. Sign the user out and clear local
state either way. The two halves are reported separately on purpose: a client
told "deleted" while the sign-in still works cannot act on the difference.

Once a deletion is under way, any other request from that user answers `409`
with code `account_deletion_pending`. Treat it as terminal for the session:
sign out, do not retry.

## Categories (built)

`category` and `subcategory` come from a taxonomy that **grows on its own**.
A weekly curator can activate a new category without an app release, so the
list the app sees is not fixed at build time.

- Drive the filter UI from `GET /api/v2/reels/category-filters`, never from a
  hard-coded list.
- An unknown category must render as itself, not as "Other" and not as a crash.
- Category colours: the app's `app_theme.dart` map is keyed by category name,
  so a new category has no colour. Fall back to a neutral rather than throwing.

## IDs and data

- Reel ids do not change. `user_saves.id` keeps every existing
  `public.reels.id`, so deep links, local caches and share cards keep working.
- Job `status` values are unchanged (`queued`, `processing`, `completed`,
  `failed`, `dead_lettered`); on terminal failures match `failure_code`, which
  is stable, not the message.
- `collection_ids` on a job is always an array, never null.

## Auth

Unchanged: the same Supabase project, the same access token in
`Authorization: Bearer`. Go verifies it locally, so nothing about token refresh
changes in the app. `user_id` in bodies or query strings is ignored — remove it
from requests when convenient, but sending it breaks nothing.

## Still to come (this file will grow)

- **`collection_ids` on a submission is still rejected**, not ignored: a
  non-empty array answers `422` naming `collection_ids`. Collections themselves
  are built; filing *at submission time* is the one piece still open, because a
  save does not exist until processing finishes. File after the job completes,
  through `POST /api/v2/collections/{collection_id}/items`, until this line
  changes.
- Search (Task 20) is the last product contract still to land here.
- The dev base URL becomes real at Task 22.
- Task 30 is the app migration itself: vendor the released OpenAPI artifact,
  regenerate the Dart client, and point dev + extensions at
  `api-dev.reelpin.in`.
