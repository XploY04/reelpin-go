package ratelimit

import "time"

// Submission started at the plan's 5 per hour, which was a cost guard written
// before spend was metered at all. It is 30 now, at the product owner's call,
// with the cost gate as the real ceiling. The per-IP number stays at twice the
// per-user one: below it, the IP bucket becomes the binding limit for a single
// person and raising the per-user number does nothing.
//
// Submission and search cost provider calls, so their handlers fail closed on
// ErrUnavailable. Cheap authenticated reads have no policy at all and stay
// available when Redis is down.
var (
	// Task 8 (enqueue) references these two.
	Submission   = Policy{Name: "submission", Requests: 30, Window: time.Hour}
	SubmissionIP = Policy{Name: "submission_ip", Requests: 60, Window: time.Hour}

	// Task 20 (hybrid search) references these two.
	Search   = Policy{Name: "search", Requests: 30, Window: time.Minute}
	SearchIP = Policy{Name: "search_ip", Requests: 90, Window: time.Minute}
)
