# 12. Route web product data through Go

**Status:** accepted

## Context

The web application and Flutter use the same Supabase identity. Letting the web
browser query product tables directly would create a second authorization and
business-rule path beside Go.

## Decision

Use `@supabase/ssr`, PKCE and cookie sessions in Next.js. Protected server code
forwards the current Supabase access token to ReelPin Go. Go verifies the token
and owns every product-data query and mutation.

The browser does not call Go directly and never receives database or Supabase
service-role credentials. Authenticated responses use private, no-store cache
behavior.

## Consequences

Web and Flutter share one API contract and authorization model. Go does not need
a browser CORS allowlist for the first release.

Next.js becomes a small server-side client boundary, not a second backend that
owns product rules.
