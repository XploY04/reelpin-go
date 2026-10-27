# Choosing the pipeline's text model

Why `gemini-2.0-flash-lite` had to go, what the three replacements did when
measured against the real provider, and why `gemini-3.5-flash-lite` won.

## The old model is gone, not going

`gemini-2.0-flash-lite` was the default in `internal/ai/gemini.go` for
transcription, extraction and categorization. It does not work any more.

Two facts that look contradictory and are not:

- `GET /v1beta/models/gemini-2.0-flash-lite` still answers `200`. The model card
  is still served.
- `POST /v1beta/models/gemini-2.0-flash-lite:generateContent` answers `404`, with
  this body:

```
This model models/gemini-2.0-flash-lite is no longer available. Please update
your code to use models/gemini-3.5-flash-lite for the latest features and
improvements.
```

It is also absent from `GET /v1beta/models`, which lists 54 models.

So the metadata endpoint outliving the inference endpoint is what makes this
look like a pending deprecation when it is a completed removal. Over two full
evaluation runs, 160 of 160 calls to it answered `404` in about 190ms. Nothing in
this repository serves traffic yet, so no user saw it, but every transcribe,
extract and categorize call the Go pipeline would make was already failing.

The provider's own message names `gemini-3.5-flash-lite`. That is a hint, not
evidence, and it is not why the model below was chosen.

## What was measured

Three candidates, all listed by the API, all with 1,048,576 input and 65,536
output token limits:

- `gemini-2.5-flash-lite`
- `gemini-3.1-flash-lite`
- `gemini-3.5-flash-lite`

`gemini-flash-lite-latest` resolves and works, and was excluded on purpose. It is
a floating alias. `content_versions.model_version` records the exact model behind
every extraction, and an alias that moved underneath would change extraction with
nothing in the data saying it had.

The harness is `internal/ai/modeleval_integration_test.go`. Each run puts 20
reels from `internal/search/testdata/corpus-v1.json` through `extract` three
times each, then through `categorize` once each: 60 extract calls and 20
categorize calls per model per run, one request at a time, temperature 0, through
the shipped `internal/ai` client with the production 45-second timeout.

Both structured calls go through `generate` with the real `responseSchema`, and
the raw text is checked against that schema before anything decodes it: every key
must be one the schema declares, every declared field must hold the declared
type, `title` and `summary` must be present, and the answer must not arrive
wrapped in a markdown fence. That check is stricter than `encoding/json`, which
would quietly ignore an undeclared field and leave a schema breach invisible.

`gemini-2.5-flash-lite` and `gemini-3.1-flash-lite` were run twice.
`gemini-3.5-flash-lite` was run four times.

| | 2.5-flash-lite | 3.1-flash-lite | 3.5-flash-lite |
|---|---|---|---|
| Calls made | 160 | 160 | 320 |
| Calls that failed | 54 (34%) | 6 (4%) | 0 |
| Schema violations, over successful calls | 0 of 106 | 0 of 154 | 0 of 320 |
| Unusable extractions after `Normalize` | 0 | 0 | 0 |
| Extract latency p50 | 1.35s, 1.39s | 1.82s, 1.65s | 1.31s to 1.35s |
| Extract latency p95 | 45.0s, 45.0s | 2.43s, 3.07s | 1.81s to 1.88s |
| Extract latency max, any run | 45.0s | 45.0s | 2.25s |
| Category matched the label | 17, 17 of 20 | 19, 19 of 20 | 18, 19, 19, 19 of 20 |
| Subcategory matched | 12, 12 of 20 | 10, 10 of 20 | 10, 13, 14, 15 of 20 |
| Category outside the taxonomy | none | none | none |

Every failure in that table is the same failure: the request was still waiting
for response headers when the 45-second client timeout fired. Not a 429, not a
5xx, no `Retry-After`. The provider simply never answered.

The counts reproduced exactly. `gemini-2.5-flash-lite` failed 27 of 80 calls in
both runs; `gemini-3.1-flash-lite` failed 3 of 80 in both; `gemini-3.5-flash-lite`
failed none of 80 in four. Token totals were identical run to run, which is what
temperature 0 should give, and it means the same prompts stall every time.

Two things were ruled out before believing it. Re-running everything under
`GODEBUG=http2client=0` changed nothing, so it is not Go's HTTP/2 connection
being shared across calls. A separate 20-call probe written in Python against
`urllib`, touching none of this repository's code, reproduced it: 9 of 20 stalls
on `gemini-2.5-flash-lite` over 420 seconds, 1 of 20 on `gemini-3.1-flash-lite`
over 79 seconds, 0 of 20 on `gemini-3.5-flash-lite` over 29 seconds.

## Schema conformance did not differ

This is the answer to the question that mattered most, and it is boring: across
580 successful structured calls over three models, there was not one schema
violation. No undeclared key, no wrong type, no missing `title` or `summary`, no
markdown fence, nothing that failed `Extraction.Validate` after `Normalize`.

`stripFences` in `internal/ai/gemini.go` never had anything to strip. It stays,
because it costs four lines and the run that needs it is the one nobody measured.

Conformance is therefore not what separates these models. Answering at all is.

## Extraction quality did not separate them either, and here is why

The intended quality measure was place recall: of the places each corpus reel is
labelled with, how many the extraction names. It is reported here because it was
measured, not because it decides anything.

| | 2.5-flash-lite | 3.1-flash-lite | 3.5-flash-lite |
|---|---|---|---|
| Places found, of places labelled | 0 of 78 | 0 of 138 | 16, 16, 19, 24 of 147 |

Do not read that as `gemini-3.5-flash-lite` being better at places. Read it as
the measure not working on this corpus. A 20-reel single-pass probe of the same
prompt showed why: **0 of 20** reels drew any location at all from
`gemini-2.5-flash-lite`, **0 of 20** from `gemini-3.1-flash-lite`, and **1 of 20**
from `gemini-3.5-flash-lite`. All three models mostly return no locations, and
they are right to. The extraction prompt tells the model to keep only places a
user could visit and would want as a map pin, and to drop cities named as
context. The corpus reels are one-line summaries whose labelled places are mostly
cities and neighbourhoods: `Artjuna`, `Anjuna`, `Goa`. Two of those three are
exactly what the prompt excludes.

The `gemini-3.5-flash-lite` figure also moved between 16 and 24 across four runs
at temperature 0, on identical inputs. A metric with that spread and that floor
cannot rank three models.

Categorization is the one quality signal that held, and it holds weakly.
`gemini-3.5-flash-lite` got 18, 19, 19 and 19 categories of 20 across its four
runs, against a flat 19 for `gemini-3.1-flash-lite` and a flat 17 for
`gemini-2.5-flash-lite`. On subcategory it ranged 10 to 15 against a flat 12 and
a flat 10, so it is ahead of `gemini-2.5-flash-lite` on average and behind it on
its worst run.

The one solid result is negative and worth more than the rest: in every
categorize call that answered, across all three models, not one picked a category
outside the taxonomy it was handed.

**So the corpus does not settle extraction quality.** It was built to judge
search, and its reels are one-line summaries rather than transcripts. Judging
extraction properly needs a labelled extraction set against real transcripts,
which does not exist yet.

## Transcription

Measured, on a small file rather than a committed fixture. 10.3 seconds of
speech, one speaker, mono, 16kHz, 32kbps MP3, 41KB, naming a cafe, a
neighbourhood, a state and two clock times.

The reference line is "We came to Artjuna cafe in Anjuna, Goa".

| Model | Runs | Wall time | Input tokens | Output tokens | What it made of the place names |
|---|---|---|---|---|---|
| 2.5-flash-lite | 2 | 2.98s, 2.24s | 369 | 41 | "Argentina cafe in Argentina", both runs |
| 3.1-flash-lite | 2 | 2.24s, 1.66s | 294 | 48 | "Arjuna Cafe in Anjuna, Goa", both runs |
| 3.5-flash-lite | 3 | 1.50s, 1.45s, 1.55s | 294 | 50, 50, 51 | "Angena", "Anjina", "Angeta" |

On tokens the three agree closely. `transcriptionPrompt` counts 37 tokens on its
own, so the 294 the two newer models were billed is about 257 tokens for 10.3
seconds of audio, near 25 tokens a second. `docs/cost-gate.md` carries that
figure, and one clip is all it rests on.

Every model got the ordinary words and both clock times right, and every model
was byte-identical run to run except the winner. They differ on proper nouns,
which is the part that matters here, because a place name is what becomes a map
pin. `gemini-3.1-flash-lite` was the only one to place the clip correctly, giving
"Anjuna, Goa" both times and missing only the `t` in the cafe's name.
`gemini-2.5-flash-lite` turned an Indian neighbourhood into "Argentina" twice and
dropped the state. `gemini-3.5-flash-lite` produced a different wrong spelling of
the neighbourhood on each of its three runs and dropped the state every time.

This does not overturn the choice, and it is one clip. The extraction prompt
already instructs the model to correct phonetic spelling mistakes in place names,
so a garbled transcript has a second chance downstream. But it is the one place
`gemini-3.5-flash-lite` did not come first, and a proper transcription set with
real reel audio would be worth having before anyone trusts place names end to
end.

## The decision

`gemini-3.5-flash-lite`.

It is not a close call, and it is not the version number that decides it. It is
the only candidate that answered every one of 320 calls. `gemini-2.5-flash-lite`
failed a third of its calls against the production timeout, and a pipeline stage
retries at most three times (`maxStageExecutions`), so a 34% per-call failure
rate leaves roughly 4% of jobs failing outright while every retry is billed.
`gemini-3.1-flash-lite` is far better at 4%, but "far better than unusable" is
not the bar, and it still put calls into the 45-second timeout in both runs. Its
p95 was 2.43s and 3.07s against 1.81s to 1.88s for the winner.

On everything else `gemini-3.5-flash-lite` is level or ahead: the fastest p50,
by far the tightest tail, category accuracy level with the best of the others,
and no schema violation anywhere. It is also the least verbose of the three that
work, which is what the output half of the bill is made of. Per successful call
`gemini-3.1-flash-lite` averaged 198 output tokens against 133, half again as
many, for no measured gain.

Two smaller things point the same way and neither was needed. The provider's own
404 message names this model. And `internal/taxonomy` already pins
`gemini-3.5-flash-lite` for the weekly curator, so the two model constants in
this repository now name the same version without being wired to each other,
which is deliberate: `taxonomy.CuratorModel` stays pinned separately so a
taxonomy decision does not move because extraction was upgraded.

## No version was bumped

`ai.PromptVersion`, `ai.SchemaVersion` and `pipeline.ProcessorVersion` are all
unchanged, on purpose.

The model is already recorded per row. `internal/pipeline/persist.go` writes
`model_version` into `reelpin.content_versions` from `Gemini.ModelVersion()`,
which returns the configured model string. Two extractions made by two different
models are already distinguishable by the column that exists for exactly that,
so a version bump would add nothing that is not there.

Each of the three would also be an active lie:

- `PromptVersion` describes the prompt text in `internal/ai/prompts.go`. Not one
  character of it changed. Bumping it would tell a later reader the prompt moved.
- `SchemaVersion` describes the shape of `Extraction`. That shape is untouched,
  and the measurement above is the evidence: the new model fills the same schema,
  320 times, without a single deviation.
- `ProcessorVersion` identifies the pipeline's stage graph, and it is mixed into
  `InputHash(state.Identity.NormalizedURL, ProcessorVersion)` for the prepare
  checkpoint. Bumping it invalidates every prepare checkpoint in flight for a
  change that happens three stages later.

One consequence to know about, which was looked at and left alone. The extract
checkpoint hashes `InputHash(transcript, caption, PromptVersion, SchemaVersion)`
and the categorize checkpoint hashes the prompt version and the taxonomy; neither
includes the model. A run that extracted under the old model and then resumed
after this deploy would reuse the old extraction from its checkpoint. Checkpoints
are scoped to one run, so this only touches runs already in flight across the
deploy, and the alternative is invalidating every checkpoint for every run to
catch them. Not worth it. Drain the queue before deploying if it matters.

## Reproducing it

```
GEMINI_API_KEY=... go test -tags=integration -count=1 -timeout 30m \
  -run TestCandidateModelsAgainstTheLiveProvider -v ./internal/ai
```

Without a key it skips, which is what CI does. Budget about 27 minutes for all
three: most of it is `gemini-2.5-flash-lite` sitting in 45-second timeouts, and
`gemini-3.5-flash-lite` alone finishes in under two.

Transcription needs an audio file, which this repository does not carry, because
a committed media fixture is megabytes every clone pays for:

```
GEMINI_API_KEY=... REELPIN_EVAL_AUDIO=/path/to/clip.mp3 go test -tags=integration \
  -count=1 -run TestCandidateModelsTranscribe -v ./internal/ai
```

The clip measured above was generated locally with macOS `say` piped through
`ffmpeg`. It was never committed.

## What this does not settle

- **Extraction quality is not measured.** The corpus judges search, not
  extraction, and place recall bottomed out for every candidate. The choice rests
  on availability, reliability, latency, schema conformance and categorization
  accuracy. It does not rest on extraction quality, and nobody should claim it
  does until a labelled extraction set over real transcripts exists.
- **One 10-second audio clip is not a transcription evaluation**, and the winner
  was the weakest of three on the proper nouns in it.
- **Why `gemini-2.5-flash-lite` stalls is unexplained.** It reproduces across two
  Go runs and an independent Python probe, on one API key, from one machine, on
  one day. It could be capacity for that model rather than anything intrinsic.
  The conclusion drawn is only that it is not fit to sit behind a 45-second
  timeout right now.
- **The corpus is 61 synthetic reels.** The same ceiling `api/eval/REPORT.md`
  names applies here.
- **The embedding model was not touched.** `gemini-embedding-2` is listed,
  available, and out of scope.
