# 5. Share Supabase Auth across Flutter and web

**Status:** accepted

## Context

The Flutter app already uses Supabase Auth. Creating a separate web identity
would split one person's library across accounts and require account linking.

## Decision

Flutter and ReelPin web use the same Supabase Auth project in each environment.
The Next.js application uses `@supabase/ssr`, PKCE and cookie sessions. Protected
pages verify the session, then send the Supabase access token to ReelPin Go.

Go verifies the JWT locally from the project's JWKS and uses `sub` as the user
identifier. Product data is read and changed through Go. The browser never
receives a service-role or secret key.

## Consequences

The same login opens the same library on mobile and web. Authorization rules,
rate limits and product behavior stay in one backend.

Authenticated Next.js pages cannot use ISR or shared CDN caching. Development
and production Vercel environments must point at their matching Supabase and Go
API resources.
