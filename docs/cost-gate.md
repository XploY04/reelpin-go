# The cost gate

What a job costs, what the gate does about it, and the three numbers the product
owner has to approve before any of it switches on.

**Nothing is switched on yet.** The mechanism is built and tested. The amounts
are not set, so the gate is off, and `reelpin_cost_gate_warn_usd` and
`reelpin_cost_gate_stop_usd` both read zero. Provider usage is still measured and
stored while the gate is off, which is how the amounts below get filled in with
real numbers instead of estimates.

## What one job actually calls

Read straight out of the code, not from memory.

A run goes through prepare, download, transcribe, extract, categorize, persist
(`internal/pipeline/pipeline.go`), and then the index event embeds the new
content version (`internal/embed/indexer.go`).

| Stage | Light job (a web page, X, Reddit, LinkedIn, Pinterest) | Media job (Instagram, YouTube, TikTok) |
|---|---|---|
| prepare | 1 page fetch, or 1 Apify actor run for X, Reddit and LinkedIn | 1 page fetch, plus 1 Apify actor run for YouTube subtitles and for the Instagram download fallback |
| download | none | bandwidth only, no provider fee unless the Apify actor served the file |
| transcribe | none: the page text is used as the transcript | 1 Gemini call with the audio inline, plus 1 more when the post is images |
| extract | 1 Gemini call | 1 Gemini call |
| categorize | 1 Gemini call | 1 Gemini call |
| index | 1 Gemini embedding of one document | 1 Gemini embedding of one document |

So: a light job is **2 Gemini text calls and 1 embedding**, sometimes plus one
actor run. A media job is **3 or 4 Gemini text calls and 1 embedding**, plus an
actor run on YouTube and on the Instagram fallback. A YouTube video whose
subtitles the actor returns skips the download and the transcribe call, so it
lands closer to a light job than a media one. The models are `gemini-3.5-flash-lite` for
every text call (`internal/ai/gemini.go`) and `gemini-embedding-2` for the index
(`internal/config/config.go`). The text model replaced `gemini-2.0-flash-lite`,
which the provider has removed; `docs/model-migration.md` has the measurements
behind the choice, including the token counts below.

The gap between the two is the transcribe call. It sends the audio inline as
base64, so its input token count is the audio's, not a sentence's. That is why
the recommended stop order below sheds media before light.

**Search costs money too.** Every search embeds its query
(`internal/search/service.go`), which is one `gemini-embedding-2` call per
search, recorded by the API against the same month. Ten searches is ten
embeddings, the same as ten saved reels. The gate never refuses a search: reads
stay available, and one embedding is the cheapest thing this service does.

Retries multiply it. A stage runs at most three times before the run fails
(`maxStageExecutions` in `internal/pipeline/pipeline.go`), so a stage that keeps
failing at the provider can bill three times for one job. Checkpoints mean an
earlier stage is not re-billed on a resume.

## What the gate does

1. Every provider call is recorded in `reelpin.provider_usage`, priced at the
   rates in force when it was made. Gemini reports `usageMetadata`, so its rows
   carry the provider's own token counts. Apify reports nothing on the endpoint
   this service uses, so its rows are a call count and say so.
2. The API sums the current calendar month on every submission and compares it
   against the approved ladder.
3. Below the warning amount, nothing changes.
4. From the warning amount up to the hard stop, the groups in the stop order shed
   new submissions one at a time, evenly spaced. With four groups between $12 and
   $20, that is one group every $2.
5. At the hard stop, every provider-costing submission answers
   `503 spend_limit_reached`.

Reads are never consulted against the gate. Neither are jobs that already have a
run: the money for a committed job is already spent, and killing it mid-flight
would spend it and throw the result away. The gate is only about new work.

## The three values to approve

| Value | Variable | Status |
|---|---|---|
| Monthly warning amount | `COST_GATE_WARN_USD` | **needs approval** |
| Monthly hard stop | `COST_GATE_STOP_USD` | **needs approval** |
| Stop order | `COST_GATE_STOP_ORDER` | **needs approval**; recommendation below |

Set all four variables (these three plus `COST_GATE_PRICES`) and the gate runs.
Set none and it is off. Set some and the API refuses to start, because a limit
assembled out of defaults is a limit nobody made.

## Inputs, and which of them are guesses

| Input | Value | Where it comes from |
|---|---|---|
| Gemini text calls per light job | 2 | Verified: `internal/pipeline/pipeline.go` |
| Gemini text calls per media job | 3, or 4 when the post carries images | Verified: `internal/pipeline/pipeline.go` |
| Embeddings per saved version | 1 | Verified: `internal/embed/indexer.go` |
| Embeddings per search | 1 | Verified: `internal/search/service.go` |
| Apify runs per job | 0 or 1, by platform | Verified: `internal/platform/*` |
| Maximum retries per stage | 3 | Verified: `maxStageExecutions` |
| Text model | `gemini-3.5-flash-lite` | Verified: `internal/ai/gemini.go` |
| Embedding model | `gemini-embedding-2` | Verified: `internal/config/config.go` |
| Gemini price per million input tokens | **unknown** | Not in this repository. Read it off the current price list and put it in `COST_GATE_PRICES`. |
| Gemini price per million output tokens | **unknown** | Same. |
| Gemini embedding price | **unknown** | Same. Price it per call if the provider does not bill embeddings by token. |
| Apify price per actor run | **unknown** | Depends on the actors in `APIFY_ACTORS` and the plan on the account. |
| Input tokens for a transcribe call | 37 prompt + about 25 per second of audio | Measured on one 10.3s clip: 294 input, 50 output, and the prompt counts 37 on its own (`docs/model-migration.md`). One clip is not a distribution. A 60-second reel works out near 1,540 input tokens, which is arithmetic, not a measurement. |
| Input tokens for an extract call | 891 input, about 170 output, mean | Measured: 60 calls over 20 corpus reels (`docs/model-migration.md`). The prompt template counts 830 with no content in it, so that is the floor and a real transcript is nearly all of what sits above it. The corpus reels contribute about 60 tokens each; a real transcript contributes far more. |
| Input tokens for a categorize call | 391 input, 20 output, mean | Measured the same way, against a 3-category, 9-subcategory taxonomy. It grows with the taxonomy, because the whole tree goes into every prompt. |
| Submissions per day, p50 and p95 | **unknown** | The plan expects fewer than 100 per day. The real figures come from production. |
| Searches per day | **unknown** | Comes from production. Watch `reelpin_search_results_total`. |

Every unknown above is a named input, not a placeholder. None of them is
guessed anywhere in the code.

## Working out the amounts

Once the price list is filled in and dev has run enough real jobs for the token
counts to settle:

```
cost per light job  = 2 × (extract+categorize tokens) × text price + 1 embedding
cost per media job  = cost per light job + transcribe tokens × text price
                      + actor run price where the platform uses one
monthly floor       = 30 × daily submissions × weighted cost per job
                      + 30 × daily searches × embedding price
```

Weight by the platform mix you actually see, not an even split: Instagram is
most of it today and it is the expensive shape.

Then:

- **The hard stop is what you are willing to lose in a bad month**, not a
  forecast. A forecast that gets multiplied by a retry storm is not a limit.
- **The warning amount is the point where there is still time to act.** For
  submissions to keep flowing while somebody looks, it wants to be far enough
  below the stop that the ladder has room; the ladder divides the space between
  the two, so a warning set at 95% of the stop gives you almost none.

## Recommended stop order

```
COST_GATE_STOP_ORDER=instagram,media,light,all
```

The reasoning, for approval or rejection:

- **`instagram` first.** It is the only platform that pays twice: an Apify actor
  run on the download fallback and then a full-length audio transcribe.
- **`media` second.** YouTube and TikTok have the transcribe call without the
  actor run, so they are cheaper than Instagram and dearer than everything else.
- **`light` third.** Two short text calls and an embedding. Shedding these buys
  the least money per user annoyed, so it goes late.
- **`all` last, and it must be last.** It matches everything, including a
  platform added after this list was written. Without it a new handler would keep
  spending past the hard stop, so the service refuses a stop order that does not
  end with it.

An order can name a platform (`instagram`) or a work class (`media`, `light`).
Whichever matches first in the list wins.

## What the gate does not cover

- Bandwidth for media downloads.
- The database, Redis, RabbitMQ and the host. Those are fixed monthly costs, not
  per-job ones.
- Firebase push delivery, which is free at this volume.
- An Apify run that was billed and then failed before returning. The client only
  records a run that answered, so this undercounts. It shows up as a gap between
  the Apify invoice and `reelpin_provider_calls_total{provider="apify"}`; if the
  gap matters, the fix is to record the run before reading the response.

## Once it is on

`docs/reopening-submissions.md` is the runbook for the day it trips.
