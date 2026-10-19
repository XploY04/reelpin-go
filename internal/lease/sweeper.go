package lease

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepInterval is deliberately slow: the sweep exists for crashed workers,
// not as a scheduler.
const SweepInterval = 30 * time.Second

// Sweeper returns expired-lease runs to their queues. It sweeps once when it
// starts, because a worker that died with the whole fleet left no marker
// behind, and afterwards while a lease is in flight or when woken.
type Sweeper struct {
	inflight *Inflight
	logger   *slog.Logger
	sweep    func(ctx context.Context) (int, error)
	// followUp is how long after a startup that found no marker to sweep
	// once more. A host back before its dead run's lease expired finds that
	// lease still live, and a Redis without persistence lost the marker.
	followUp time.Duration
	wake     chan struct{}
}

func NewSweeper(pool *pgxpool.Pool, inflight *Inflight, signal *outbox.Signal, fallbackRoutingKey string, logger *slog.Logger) *Sweeper {
	return &Sweeper{
		inflight: inflight,
		logger:   logger,
		sweep: func(ctx context.Context) (int, error) {
			return SweepExpired(ctx, pool, signal, fallbackRoutingKey, 100)
		},
		followUp: Duration,
		wake:     make(chan struct{}, 1),
	}
}

// Wake asks for a sweep now. It never blocks: a wake already pending covers
// this one.
func (s *Sweeper) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run sweeps until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = SweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.sweepOnce(ctx)
	// due is false only when Redis answered that nothing is in flight; in
	// every other case the ticks keep sweeping without help.
	var followUp <-chan time.Time
	if !s.due(ctx) {
		timer := time.NewTimer(s.followUp)
		defer timer.Stop()
		followUp = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-followUp:
			s.sweepOnce(ctx)
		case <-s.wake:
			s.sweepOnce(ctx)
		case <-ticker.C:
			if s.due(ctx) {
				s.sweepOnce(ctx)
			}
		}
	}
}

func (s *Sweeper) due(ctx context.Context) bool {
	if s.inflight == nil {
		return true
	}
	active, err := s.inflight.active(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Warn("lease marker unreadable, sweeping anyway", "error", err)
		}
		return true
	}
	return active
}

func (s *Sweeper) sweepOnce(ctx context.Context) {
	swept, err := s.sweep(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Error("lease sweep failed", "error", err)
		}
		return
	}
	if swept > 0 {
		s.logger.Info("requeued runs from workers that went away", "count", swept)
	}
}
