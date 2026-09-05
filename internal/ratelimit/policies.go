package ratelimit

import "time"

// The values are the implementation plan's, defined here as soon as they are
// known so the task that adds the endpoint only has to reference them.
//
// Submission and search cost provider calls, so their handlers fail closed on
// ErrUnavailable. Cheap authenticated reads have no policy at all and stay
// available when Redis is down.
var (
	// Task 8 (enqueue) references these two.
	Submission   = Policy{Name: "submission", Requests: 5, Window: time.Hour}
	SubmissionIP = Policy{Name: "submission_ip", Requests: 20, Window: time.Hour}

	// Task 20 (hybrid search) references these two.
	Search   = Policy{Name: "search", Requests: 30, Window: time.Minute}
	SearchIP = Policy{Name: "search_ip", Requests: 90, Window: time.Minute}
)
