# Search evaluation, set `search-eval-v1`

What this measures, how to reproduce it, and what is still missing before hybrid
search can be turned on for real users.

## The set

`api/eval/search-v1.json` holds 8 judged queries over the 6-reel library in
`internal/search/testdata/corpus-v1.json`. Judgments are keyed by reel URL, so
the set survives a rebuilt database. Gains are 3 (what the user meant), 2 (a
good alternative) and 1 (related).

The set is versioned. Adding a query is an append. Changing an existing
judgment means a new version, because two reports are only comparable when they
came from the same judgments.

## Reproducing it

Offline, no credentials, runs in CI:

```
docker compose up -d
TEST_DATABASE_URL=postgres://reelpin:reelpin@localhost:5432/reelpin \
  go test -tags=integration -run TestTheLabeledSetRunsAgainstTheCorpus -v ./internal/search
```

Against a real library, with real Gemini embeddings:

```
maintenance search-eval --user <uuid> --out report.json
maintenance search-eval --user <uuid> --max-distance 0.5   # a tuning run
```

**The set judges specific reels by URL**, so it only measures anything against a
library that holds them. Either seed `internal/search/testdata/corpus-v1.json`
into a dedicated evaluation user, or pass `--set` with a set labeled with the
chosen user's own reel URLs. The command checks coverage first and refuses below
80% (`--min-coverage`), because a library missing the judged reels scores zero
everywhere and reads as a search regression when it is nothing of the kind.

## What the offline run measured

Both rows are the same set, same corpus, same database. The only difference is
whether the vector arm ran.

| System | P@5 | Recall@10 | MRR | NDCG@10 | Zero-result rate | p50 | p95 |
|--------|-----|-----------|-----|---------|------------------|-----|-----|
| Hybrid (dense + sparse + fuzzy) | 0.225 | 0.875 | 0.875 | 0.875 | 0% | 1.3ms | 3.7ms |
| Lexical only (sparse + fuzzy) | 0.100 | 0.438 | 0.500 | 0.490 | 50% | 0.4ms | 0.7ms |

Recall doubles and MRR rises from 0.50 to 0.875. The two queries lexical search
cannot answer at all are the ones it should not be able to: a description with
no name in it, and a detail that lives in an extracted fact rather than in the
title, summary or caption the full-text column is built from.

Read these numbers as plumbing evidence, not relevance evidence. The offline run
substitutes a bag-of-words stand-in for Gemini, so its "dense" arm is lexical
too. It proves the arms, the fusion, the SQL filters and the metrics work end to
end. It does not predict what Gemini will rank.

The stand-in's vectors also sit far outside the cosine range the relevance gate
is tuned for, so the offline hybrid run opens the gate
(`service.MaxDistance = 1`). That is why its zero-result rate is 0: with the
gate open, the vector arm always returns its nearest neighbours. With the
shipped gate the same unanswerable query returns nothing, which
`TestAQueryAboutSomethingUnsavedComesBackEmpty` covers directly.

## Latency and cost

p50 and p95 above are the search service end to end, including all three arms
and the final reload, against a 6-reel library on a local Postgres. They are a
floor, not a budget: a real library is larger and the Gemini call is not in
them.

Per query, hybrid search costs exactly one embedding call when the vector arm
runs, and zero when it degrades. `dense_queries` in the report is that call
count. A degraded query is free and still returns results.

## What is still missing

Two of the three systems the migration task asks to compare cannot be measured
from here:

- **Python hash/Pinecone.** Needs production Pinecone credentials and the
  production index. Not runnable in this environment.
- **Hybrid with real Gemini embeddings.** Needs a `GEMINI_API_KEY` and a real
  library. `maintenance search-eval` is the command; it has never been run.

The task also asks for at least 50 labeled queries reaching Recall@10 0.85 and
NDCG@10 0.75. This set has 8, over invented content. It is enough to prove the
harness, not the evidence the gate asks for; the real set has to be labeled
against a real library.

Until all of that exists, hybrid search should stay behind whatever switch the
cutover uses. The acceptance bar is unchanged: better relevance than the
Pinecone path, without breaking the latency and cost budget.

## Tuning the gate

`search.MaxDenseDistance` is 0.42 cosine distance, which is 0.58 similarity,
the lower semantic bar the Python Pinecone path already used
(`_is_relevant_match`). It is a `Service` field rather than a constant because
the right cutoff depends on the embedding model. Sweep it with `--max-distance`
and watch the zero-result rate move against recall: too low and real matches
disappear, too high and every query returns something.
