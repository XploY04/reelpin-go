package entitlements

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ActiveProReader loads every user whose Pro grant is still in force, mapped to
// when it expires. It is the one storage port this package needs.
type ActiveProReader interface {
	ActivePro(ctx context.Context) (map[string]time.Time, error)
}

// Cache answers IsPro without a query per request. It reloads the whole active
// set no more often than ttl, and a reload that fails keeps the last good set
// rather than dropping paying users during a database outage.
type Cache struct {
	reader ActiveProReader
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu         sync.RWMutex
	expiries   map[string]time.Time
	lastReload time.Time
	loaded     bool
}

// NewCache builds a cache. A zero ttl means reload on every call, which is only
// useful in a test.
func NewCache(reader ActiveProReader, ttl time.Duration, now func() time.Time, logger *slog.Logger) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{reader: reader, ttl: ttl, now: now, logger: logger}
}

// IsPro reports whether the user has a Pro grant that has not expired. A user
// absent from the loaded set, or one whose grant has since lapsed, is not Pro.
func (c *Cache) IsPro(ctx context.Context, userID string) bool {
	c.refreshIfStale(ctx)

	c.mu.RLock()
	defer c.mu.RUnlock()
	expiry, ok := c.expiries[userID]
	return ok && expiry.After(c.now())
}

func (c *Cache) refreshIfStale(ctx context.Context) {
	c.mu.RLock()
	fresh := c.loaded && c.now().Sub(c.lastReload) < c.ttl
	c.mu.RUnlock()
	if fresh {
		return
	}

	// Claim the window before querying so only one goroutine reloads per
	// interval; the rest read the last good set. A failed reload keeps that set
	// and the claim stands, so a sustained outage costs one query per window,
	// not one per request. On a cold start with nothing cached, the set stays
	// empty, which reads every user as Free until the first successful load.
	c.mu.Lock()
	if c.loaded && c.now().Sub(c.lastReload) < c.ttl {
		c.mu.Unlock()
		return
	}
	c.lastReload = c.now()
	c.loaded = true
	c.mu.Unlock()

	// The reload is shared by every caller in the window, so it must not die with
	// the request that happened to trigger it.
	loaded, err := c.reader.ActivePro(context.WithoutCancel(ctx))
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("entitlements reload failed; keeping the last good set", "error", err)
		}
		return
	}

	c.mu.Lock()
	c.expiries = loaded
	c.mu.Unlock()
}
