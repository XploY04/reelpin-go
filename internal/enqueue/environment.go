package enqueue

// DefaultEnvironment is what an unset environment falls back to. It is
// production on purpose: the failure it prevents is a dev process claiming
// production work, and defaulting to development would make an unconfigured
// process do exactly that.
const DefaultEnvironment = "production"
