# Domain model

The two things this service is about, and the rules that are easy to get wrong.

## Reels and jobs

A **reel** is one user's saved piece of content: a link plus everything
extracted from it. A **processing job** is one attempt to turn a shared link
into a reel. They are separate rows with separate lifetimes, and the app polls
the second to learn about the first.

```
share a link ──▶ processing job ──▶ (pipeline) ──▶ reel
                      │                             ▲
                      └── result_reel_id ───────────┘
```

`ReelRecord` and `JobRecord` in `internal/reels` and `internal/jobs` are the
shapes as stored. `DisplayReel` and the job response are the shapes as served;
the app sees only the second kind, and the builders between them
(`BuildDisplayReel`, `BuildListResponse`) are where every presentation decision
lives.

## Rules

**A reel belongs to exactly one user.** There is no sharing at this layer. Every
query filters by `user_id`, and that id always comes from the verified token.
Two users saving the same link today produce two independent reels, two
downloads and two of everything. Removing that duplication is a later layer and
a schema change, not something to improvise in a query.

**A job that completed without a reel is not a success.** `status = completed`
with a null `result_reel_id` is a job the app would poll forever, so it is
presented as failed. The row is left alone; see
[`decisions/0002-reads-never-write.md`](decisions/0002-reads-never-write.md).

**A location without coordinates is not mappable.** `BuildMappableLocations`
drops any location missing a latitude or longitude rather than emitting a pin at
zero. A reel can have locations and still have nothing to show on a map.

**Categories always have a value.** An empty category reads as `Other`, not as
an empty string, because the app groups by it.

**Job statuses are a closed set**: `queued`, `processing`, `completed`,
`failed`, `dead_lettered`. `dead_lettered` means no further attempt will be
made. The app's progress copy is derived from the status and the current step
together, in `internal/jobs/status.go`, so the two must stay consistent.

## Platforms

A reel's platform is derived from its URL host, not from anything the client
sends. `internal/reels/platform.go` owns that mapping, including the short forms
(`youtu.be`, `t.co`, `pin.it`, `lnkd.in`, `redd.it`, `instagr.am`). A host that
matches nothing is not an error: it is the `other` bucket, and filtering by
`other` means "everything not in a named platform", which is a `NOT IN` query
rather than an equality.

Adding a platform means adding to that mapping and nowhere else.

## Transcripts

The transcript is the largest field on a reel and is only ever selected by
`GET /reels/{id}`. List queries name their columns explicitly and leave it out.
This is deliberate: a list of twenty reels with transcripts is a payload the app
never reads and a cost nobody asked for.
