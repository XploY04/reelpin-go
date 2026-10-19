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
// behind, and afterwards only while a lease is in flight.
type Sweeper struct {
	inflight *Inflight
	logger   *slog.Logger
	sweep    func(ctx context.Context) (int, error)
}

func NewSweeper(pool *pgxpool.Pool, inflight *Inflight, signal *outbox.Signal, fallbackRoutingKey string, logger *slog.Logger) *Sweeper {
	return &Sweeper{
		inflight: inflight,
		logger:   logger,
		sweep: func(ctx context.Context) (int, error) {
			return SweepExpired(ctx, pool, signal, fallbackRoutingKey, 100)
		},
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
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if s.due(ctx) {
			s.sweepOnce(ctx)
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
