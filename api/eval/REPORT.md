# Search evaluation, set `search-eval-v2`

What this measures, how to reproduce it, and what it does not settle.

## The set

`api/eval/search-v1.json` holds 70 judged queries over the 61-reel library in
`internal/search/testdata/corpus-v1.json`. 64 of them have at least one right
answer; the other 6 are queries the library cannot answer, and their only
correct outcome is an empty result.

Judgments are keyed by reel URL, so the set survives a rebuilt database. Gains
are 3 (what the user meant), 2 (a good alternative) and 1 (related).

The queries are grouped by which arm has to carry them, because that is the only
grouping that changes behaviour:

| Kind | Count | What it exercises |
|------|-------|-------------------|
| Exact name and multi-word phrase | 12 | Full-text over title, summary and caption |
| Typo and partial word | 7 | Trigram similarity against the title |
| Semantic paraphrase, no shared words | 17 | The vector arm alone |
| Key fact rather than a title | 17 | The vector arm again: facts, tags and places are not in the full-text column |
| Filter shaped (platform, category, subcategory, date) | 8 | The filter narrowing inside each arm before ranking |
| Words split across fields, or a place only in metadata | 3 | Two arms together |
| Unrelated, must return nothing | 6 | The dense relevance gate |

The corpus is 61 invented reels spread across Tokyo, Osaka, Kyoto, Seoul,
Bangkok, Hanoi, Singapore, Bengaluru, Mumbai, Delhi, Goa, Amsterdam, Lisbon,
Porto, Berlin, Paris, Barcelona, Istanbul, Rome, London, Copenhagen, Reykjavik,
Iceland, Mexico City, Oaxaca, New York, Lima, Cape Town and Marrakech, plus
reels with no place at all. 11 of the 61 are never judged by any query and exist
only as distractors.

The set is versioned. Adding a query is an append. Changing an existing judgment
means a new version, because two reports are only comparable when they came from
the same judgments. `search-eval-v1` was 8 queries over 6 reels; none of its
numbers carry over.

## What was measured

Real `gemini-embedding-2` vectors at 768 dimensions, the shipped model, over the
whole checked-in set. Both rows are the same corpus and the same database. The
only difference is whether the vector arm ran at all. The lexical row is from
the offline run, because that path never calls the provider and its numbers do
not depend on which embedder is configured.

| System | P@5 | Recall@10 | MRR | nDCG@10 | Unrelated queries empty | p50 | p95 |
|--------|-----|-----------|-----|---------|-------------------------|-----|-----|
| Hybrid, gate 0.40 | 0.259 | 0.992 | 0.982 | 0.975 | 6 of 6 | 1.3ms | 1.7ms |
| Lexical only (full-text + trigram) | 0.113 | 0.427 | 0.453 | 0.445 | 6 of 6 | 0.8ms | 0.9ms |

Both quality criteria are met: Recall@10 0.992 against a bar of 0.85, nDCG@10
0.975 against a bar of 0.75, and every unrelated query comes back empty.

Read P@5 against its ceiling, not against 1. 50 of the 64 judged queries have a
single right answer, so the best P@5 the judgments allow is 0.272. The measured
0.259 is 95% of that.

Recall@10, nDCG@10, MRR and P@5 are averaged over the 64 judged queries only.
The 6 unrelated ones are counted separately, as correct-empty or not. Averaging
their unavoidable zero into recall would have made the relevance score a
function of how many unrelated queries the set happens to hold, which is a
property of the set and not of search.

One judged query is still incomplete at the chosen gate:

```
incomplete at 0.40: category-word (recall 0.50, ndcg 0.92) "sunset viewpoint"
```

It has two judgments, a 3 and a 1, and only finds the 3. The related reel, a
long vlog whose sunset is one stop among many, does not reach the top ten. nDCG
is 0.92 for that query because the answer that matters is first. No other query
kind fails: not one paraphrase, not one key-fact query, not one typo, not one
filter.

## The dense gate, measured

`search.MaxDenseDistance` was 0.42, carried over from the Python Pinecone path's
`_is_relevant_match` and never measured against this model. Decision 0011 says
thresholds are measured, so it was swept: every cutoff from 0.20 to 1.00 in
steps of 0.02, the whole set at each one, real embeddings throughout.

| Cutoff | Recall@10 | nDCG@10 | Unrelated empty |
|--------|-----------|---------|-----------------|
| 0.30 | 0.689 | 0.716 | 6 of 6 |
| 0.34 | 0.837 | 0.853 | 6 of 6 |
| 0.36 | 0.914 | 0.915 | 6 of 6 |
| 0.38 | 0.982 | 0.970 | 6 of 6 |
| **0.40** | **0.992** | **0.975** | **6 of 6** |
| 0.42 | 0.992 | 0.975 | 5 of 6 |
| 0.44 | 1.000 | 0.976 | 4 of 6 |
| 0.46 | 1.000 | 0.976 | 2 of 6 |
| 0.48 and above | 1.000 | 0.976 | 0 of 6 |

0.40 wins: it is the widest cutoff at which every unrelated query still returns
nothing. The old 0.42 is past the edge, and one unrelated query already comes
back with results there. Going wider buys 0.008 of recall and gives up the empty
answer completely, which is the wrong trade for a personal library where "you
never saved that" is a real and common answer.

Recall is flat from 0.44 upward because the vector arm has already found
everything the judgments name. Past that point the gate only decides how much
noise a hopeless query gets back.

## Reproducing it

Against the real model, which is the run this report records:

```
docker compose up -d
GEMINI_API_KEY=... TEST_DATABASE_URL=postgres://reelpin@localhost:5432/reelpin \
  go test -tags=integration -count=1 -run TestSweepingTheDenseGate -v ./internal/search
```

The test seeds the corpus, embeds every document and every query once, then
sweeps, then makes one uncached pass for the latency figure. About 200
embeddings for the whole thing: the cache is what keeps a 41-point sweep from
costing 41 times the provider bill.

Offline, no credentials, which is what CI runs:

```
TEST_DATABASE_URL=postgres://reelpin@localhost:5432/reelpin \
  go test -tags=integration -count=1 -run TestTheLabeledSetRunsAgainstTheCorpus -v ./internal/search
```

Without a key the same tests fall back to a bag-of-words stand-in embedder. That
run proves the three arms, the fusion, the SQL filters and the metrics work end
to end. It does not measure relevance, because the stand-in is lexical: it
reaches Recall@10 0.849 and nDCG@10 0.728 with the gate held open, and its
distances sit on a different scale, so the cutoff it picks must never be copied
into the constant.

Against a real user's library:

```
maintenance search-eval --user <uuid> --out report.json
maintenance search-eval --user <uuid> --max-distance 0.5   # one tuning point
```

**The set judges specific reels by URL**, so it only measures anything against a
library that holds them. Either seed the corpus into a dedicated evaluation
user, or pass `--set` with a set labeled with that user's own reel URLs. The
command checks coverage first and refuses below 80% (`--min-coverage`), because
a library missing the judged reels scores zero everywhere and reads as a search
regression when it is nothing of the kind.

## Latency and cost

The p50 and p95 in the table above are the search service end to end through all
three arms and the final reload, with the query embedding served from cache.
They are the database side only.

One uncached pass, paying a real embedding call per query, measured p50 588ms
and p95 742ms. The provider round trip is essentially all of it. That is inside
the 1.5 second development budget with room to spare, and it is the number to
watch: the SQL is not what will break the budget.

Per query, hybrid search costs exactly one embedding call when the vector arm
runs, and zero when it degrades. `dense_queries` in the report counts the
queries whose vector arm returned candidates, which is 64 rather than 70 here:
the 6 unrelated queries paid for a vector and had every neighbour rejected by
the gate. A degraded query is free and still returns results.

## What this does not settle

- **The corpus is synthetic.** 61 reels written for this evaluation, not
  production data, and the queries were written by the same person who wrote the
  corpus. That is a real ceiling on what the numbers mean. Task 28 replaces it
  with a set labeled against reels real users actually saved. Until then, read
  0.99 as evidence that the arms and the fusion work, not as the relevance a
  user will experience.
- **61 rows is not a production library.** Finding the right reel in the top ten
  of 61 is easier than in the top ten of several thousand. The task's
  "production-shaped row counts" applies to the latency evidence, not to these
  recall numbers.
- **Python hash/Pinecone was not compared.** It needs production Pinecone
  credentials and the production index, neither of which is runnable here. The
  lexical-only row is the only baseline in this report.
- **Query and document embeddings use the same task type.** `embed.Gemini` sends
  `RETRIEVAL_DOCUMENT` for both. Asymmetric retrieval normally sends
  `RETRIEVAL_QUERY` for the query side. It is not hurting these numbers, and it
  was left alone rather than changed on the branch that measures them.

The acceptance bar for the cutover is unchanged: better relevance than the
Pinecone path, without breaking the latency and cost budget. This report clears
the quality gate on a synthetic library and leaves the Pinecone comparison open.
