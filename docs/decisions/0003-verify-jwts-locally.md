# 3. Verify Supabase tokens locally, not by calling Supabase

**Status:** accepted

## Context

The Python service validates a request by asking Supabase who the token belongs
to. That is one network call on the critical path of every authenticated
request, and it makes Supabase's latency and availability our own.

Supabase signs its tokens with ES256 and publishes the public keys at
`<SUPABASE_URL>/auth/v1/.well-known/jwks.json`, so the check can be done here.

## Decision

Verify locally with `lestrrat-go/jwx/v3`. Fetch the JWKS once at startup with a
5s timeout and cache it for at most 10 minutes. A token with an unknown `kid`
may force a refresh, limited to one refresh per five-second window so an
unauthenticated caller cannot turn every request into an outbound request.

Check, in this order: that the protected header says ES256 **before**
verifying, then the signature, issuer, audience, expiry, `nbf` with 30s skew,
`role = authenticated`, and a `sub` that parses as a UUID.

Do not implement any of the cryptography by hand.

## Consequences

No per-request network call, so Supabase being slow does not make this service
slow, and a Supabase outage does not reject valid tokens for up to the cache
lifetime.

Startup now depends on Supabase being reachable, and fails loudly if it is not.
That trade is deliberate: failing at startup is visible, while lazily failing on
the first request is not.

Key rotation is picked up by the first allowed refresh for the new key. A
rotation that lands inside the five-second refresh window may be rejected once
and succeeds after the window. A revoked token stays valid until it expires,
because nothing is asked about it any more. That is the standard consequence of
stateless verification and is acceptable for tokens with the lifetimes Supabase
issues; a feature that needs immediate revocation needs a different mechanism,
not a different verification strategy.

Checking the algorithm in the header before verification is what stops a token
that claims a different algorithm from being handed to a verifier that would
accept it.
