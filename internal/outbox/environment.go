package outbox

// DefaultEnvironment is what an unset environment falls back to. It is
// production on purpose: the failure it prevents is a dev dispatcher claiming
// production rows, and defaulting to development would make an unconfigured
// process do exactly that.
const DefaultEnvironment = "production"
