package ratelimit

import "time"

// The window and the values are the ones the Python service runs today. A
// policy is defined here as soon as it is known, so the task that adds the
// endpoint only has to reference it.
const window = time.Minute

var (
	// Enqueue, search, token registration and admin actions cost money or
	// change state, so their handlers fail closed when Redis is unavailable.
	Enqueue             = Policy{Name: "enqueue", Requests: 10, Window: window}
	EnqueueIP           = Policy{Name: "enqueue_ip", Requests: 30, Window: window}
	Search              = Policy{Name: "search", Requests: 30, Window: window}
	SearchIP            = Policy{Name: "search_ip", Requests: 90, Window: window}
	TokenRegistration   = Policy{Name: "token_registration", Requests: 20, Window: window}
	TokenRegistrationIP = Policy{Name: "token_registration_ip", Requests: 60, Window: window}
	ShareTokenMint      = Policy{Name: "share_token_mint", Requests: 10, Window: window}
	ShareTokenMintIP    = Policy{Name: "share_token_mint_ip", Requests: 30, Window: window}
	AdminActionIP       = Policy{Name: "admin_action_ip", Requests: 30, Window: window}

	// Share resolution is a read: it calls no provider and writes nothing, so
	// its limit exists to stop abuse, and it fails open.
	ShareResolve   = Policy{Name: "share_resolve", Requests: 60, Window: window}
	ShareResolveIP = Policy{Name: "share_resolve_ip", Requests: 180, Window: window}
)
