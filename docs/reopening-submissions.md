# Reopening submissions after the cost gate trips

The gate has refused a submission. This is what to check and what to change.

You need the admin key to read the metrics and SSH access to the host to read
the ledger or change the limits. Both are operator credentials; nothing here is
a product feature and no client token reaches any of it.

## What the user sees

New submissions answer `503` with `error.code = "spend_limit_reached"` and
`retryable: false`. Everything already saved still reads, search still works, and
any job that was already running finishes and produces its reel. The app shows
the message and does not retry on its own.

If reads are failing too, this is not the cost gate. Go to
[`operations.md`](operations.md).

## 1. Confirm it is the gate, and which group

From the API host, the way the smoke script reaches it:

```sh
curl -s -H "X-Admin-Key: $ADMIN_KEY" http://127.0.0.1:8000/metrics \
  | grep -E 'reelpin_(provider_spend_month_usd|cost_gate_|submissions_blocked)'
```

You want four things:

- `reelpin_provider_spend_month_usd`: what the month has cost so far.
- `reelpin_cost_gate_warn_usd` and `reelpin_cost_gate_stop_usd`: the approved
  amounts. Both zero means no gate is configured and the 503 came from somewhere
  else.
- `reelpin_submissions_blocked_total{group="..."}`: which stop-order group is
  refusing. `all` means the hard stop; anything else means one group is shedding
  and the rest are still accepted.

The API logs the whole ladder at startup, so
`journalctl -u reelpin-api-production | grep "cost gate enabled"` tells you
exactly which dollar figure each group sheds at.

## 2. Decide whether the number is real

Before raising anything, check the gate is not undercounting or overcounting.

```sh
curl -s -H "X-Admin-Key: $ADMIN_KEY" http://127.0.0.1:8000/metrics \
  | grep 'reelpin_provider_calls_total' | grep -E 'unpriced|unrecorded'
```

- **`unpriced`**: a provider or model with no entry in `COST_GATE_PRICES`. Real
  spending is higher than the gauge says.
- **`unrecorded`**: the ledger insert failed. Same, and worse: those calls are
  missing from the month's total entirely.

Then look at where the month went:

```sh
psql "$DATABASE_URL" -c "
  SELECT provider, model, operation,
         sum(calls) AS calls,
         round(sum(cost_micros)/1000000.0, 2) AS usd
  FROM reelpin.provider_usage
  WHERE occurred_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
  GROUP BY 1, 2, 3
  ORDER BY 4 DESC;"
```

A month dominated by `transcribe` is normal. A month dominated by one operation
that used to be small is a retry loop: check
`reelpin_pipeline_stage_results_total` and `reelpin_provider_failures_total`
before you decide the limit is wrong.

## 3. Reopen

There are three ways, in order of preference.

### Wait

The gate sums the current calendar month. At 00:00 UTC on the first it resets to
zero and submissions reopen on their own, with no deployment. If the month is
nearly over and the volume is small, this is the right answer.

### Raise the limit

This is a spending decision, so the product owner makes it, not the operator.
Once approved, edit the environment file on the host and restart the API:

```sh
ssh reelpin
sudo -e /etc/reelpin/production.env         # COST_GATE_STOP_USD, and COST_GATE_WARN_USD with it
sudo systemctl restart reelpin-api-production
journalctl -u reelpin-api-production -n 20  # confirm the new ladder in the startup line
```

Raise the warning amount along with the stop. Leaving the warning where it is
squeezes the ladder: every group would then shed within a smaller band, which is
the opposite of what raising the limit was for.

Only the API needs restarting for an amount or a stop order: the gate lives
there. `COST_GATE_PRICES` is read by both, so a price change also needs
`sudo systemctl restart reelpin-worker-production`, or new calls keep being
priced at the old rate.

The env file is shared by the API and the worker for one environment. It is
never written by the deploy script, so an edit here survives the next release.

### Reopen one group at a time

If the spend is real but one platform is the whole problem, shorten the stop
order rather than raising the money. Moving a group later in the list, or taking
it out, changes where it sheds. Removing `all` is not allowed and the API will
refuse to start.

## 4. Correcting the ledger

Only when a row is provably wrong, for instance a price entry with a misplaced
decimal point that valued a month's calls at a hundred times their cost.

```sh
# Look before you delete. Rows are the evidence for what the month cost.
psql "$DATABASE_URL" -c "
  SELECT id, occurred_at, provider, model, operation, calls, cost_micros
  FROM reelpin.provider_usage
  WHERE provider = '...' AND occurred_at >= '...'
  ORDER BY occurred_at;"
```

Delete by id, never by a range, and write down what you deleted and why. The
invoice from the provider is the second source; if the ledger and the invoice
disagree, the invoice is right and the ledger has a bug worth fixing.

Fixing a price does not re-price old rows. That is deliberate: an earlier month
cost what it cost. If a bad price has to be corrected, delete the affected rows
and reinsert them at the right price, or accept the discrepancy and note it.

## 5. Turning the gate off

Unset all four variables and restart the API. Submissions reopen and spending is
measured but not limited. This is not a fix; it is what to do when the gate
itself is the thing that is broken, and it needs a follow-up.

```sh
sudo -e /etc/reelpin/production.env   # remove COST_GATE_WARN_USD, COST_GATE_STOP_USD,
                                      # COST_GATE_STOP_ORDER, COST_GATE_PRICES
sudo systemctl restart reelpin-api-production
sudo systemctl restart reelpin-worker-production
```

Calls are still counted and stored with the gate off; without prices they are
stored at zero and reported as `unpriced`.

Removing only some of them makes the API refuse to start. All four or none.

## After any change

Check `reelpin_cost_gate_warn_usd` and `reelpin_cost_gate_stop_usd` report the
new amounts, then submit one real link and confirm a `202`. A gate that was
reopened but still refuses is a gate that read a stale environment file.
