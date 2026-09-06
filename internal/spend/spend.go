// Package spend prices what the providers actually did and decides whether new
// provider-costing work may still be accepted. Nothing here calls a provider,
// knows about transport, or writes SQL: the ledger takes a store port and the
// gate takes a reader.
//
// Money is whole micros of one US dollar. Provider prices are quoted in
// fractions of a cent, and a float would round them differently on every
// machine that added them up.
package spend

import (
	"context"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// Micros is a millionth of a US dollar.
type Micros int64

// USD is the value as a dollar amount, for a gauge or a person. It is never
// used to add two amounts together.
func (m Micros) USD() float64 { return float64(m) / 1_000_000 }

// Usage is one provider call as the provider described it.
type Usage struct {
	Provider  string
	Model     string
	Operation string
	// Calls is what was billed as a unit: one request for a model, one actor
	// run for Apify, one document for the embedding index.
	Calls        int
	InputTokens  int64
	OutputTokens int64
	// Measured is true only when the provider reported the token counts. False
	// means Calls is all that is known, so the row can only be priced per call.
	Measured bool
}

// Entry is a priced Usage as it is stored.
type Entry struct {
	Usage
	CostMicros Micros
	// Priced is false when no configured price covers this call. The row is
	// still stored, because an unpriced call is spend nobody has valued, not
	// spend that did not happen.
	Priced bool
}

// Store is the ledger's persistence. Implemented by internal/postgres.
type Store interface {
	Insert(ctx context.Context, entry Entry) error
	MonthToDateMicros(ctx context.Context) (Micros, error)
}

// Recorder takes one usage record. It returns nothing on purpose: by the time a
// call is reported the money is already spent, and failing the job over the
// bookkeeping would waste it as well as lose it.
type Recorder interface {
	Record(ctx context.Context, usage Usage)
}

// recordTimeout bounds the insert. The caller is usually a pipeline stage whose
// own budget is nearly gone.
const recordTimeout = 5 * time.Second

// Ledger prices usage and stores it.
type Ledger struct {
	store   Store
	prices  Prices
	meters  *metrics.Metrics
	logger  *slog.Logger
	timeout time.Duration
}

func NewLedger(store Store, prices Prices, meters *metrics.Metrics, logger *slog.Logger) *Ledger {
	return &Ledger{store: store, prices: prices, meters: meters, logger: logger, timeout: recordTimeout}
}

// Record prices one call and stores it. The context is detached: a stage that
// has just been cancelled still spent the money, and a row that never lands is
// spend the gate cannot see.
func (l *Ledger) Record(ctx context.Context, usage Usage) {
	if l == nil || usage.Calls <= 0 {
		return
	}

	cost, priced := l.prices.Cost(usage)
	entry := Entry{Usage: usage, CostMicros: cost, Priced: priced}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.timeout)
	defer cancel()

	accounting := accountingLabel(usage, priced)
	if err := l.store.Insert(ctx, entry); err != nil {
		accounting = "unrecorded"
		l.logger.Error("provider usage was not recorded, so the cost gate is undercounting",
			"provider", usage.Provider, "model", usage.Model,
			"operation", usage.Operation, "error", err)
	}
	l.observe(usage, accounting)
}

// accountingLabel names the weakest thing true about a call, so one series
// answers "is the gate seeing the real number".
func accountingLabel(usage Usage, priced bool) string {
	switch {
	case !priced:
		return "unpriced"
	case usage.Measured:
		return "measured"
	default:
		return "counted"
	}
}

func (l *Ledger) observe(usage Usage, accounting string) {
	if l.meters == nil {
		return
	}
	l.meters.ProviderCalls.
		WithLabelValues(usage.Provider, usage.Model, usage.Operation, accounting).
		Add(float64(usage.Calls))
	if usage.InputTokens > 0 {
		l.meters.ProviderTokens.
			WithLabelValues(usage.Provider, usage.Model, usage.Operation, "input").
			Add(float64(usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		l.meters.ProviderTokens.
			WithLabelValues(usage.Provider, usage.Model, usage.Operation, "output").
			Add(float64(usage.OutputTokens))
	}
}
