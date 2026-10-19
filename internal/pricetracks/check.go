package pricetracks

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/XploY04/reelpin-go/internal/notify"
)

// CheckInterval is how stale a track must be before a run reads it again.
// It is shorter than a day on purpose: the timer's randomized start must not
// push a track checked at 03:01 yesterday past today's 03:00 run.
const CheckInterval = 20 * time.Hour

// checkPage is how many tracks one query hands the run at a time.
const checkPage = 100

// Quote is what a product page said about its price. A nil Price means the
// page did not say, which is normal for script-rendered or bot-blocked pages.
type Quote struct {
	Price    *float64
	Currency string
}

// Fetcher reads one product page.
type Fetcher interface {
	FetchPrice(ctx context.Context, url string) (Quote, error)
}

// Notifier is the push path. notify.Service implements it.
type Notifier interface {
	SendToUser(ctx context.Context, notification notify.Notification) (string, error)
}

// Report is what one run did.
type Report struct {
	Checked  int
	Priced   int
	Notified int
	Failed   int
}

// Checker is the daily run.
type Checker struct {
	store       Store
	fetcher     Fetcher
	notifier    Notifier
	logger      *slog.Logger
	now         func() time.Time
	concurrency int
}

func NewChecker(store Store, fetcher Fetcher, notifier Notifier, logger *slog.Logger, now func() time.Time, concurrency int) *Checker {
	if now == nil {
		now = time.Now
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return &Checker{store: store, fetcher: fetcher, notifier: notifier, logger: logger, now: now, concurrency: concurrency}
}

// CheckDue reads every active track not checked within CheckInterval. It
// pages by id rather than by staleness, so a track whose check could not be
// recorded is not handed back to this same run forever.
func (c *Checker) CheckDue(ctx context.Context) (Report, error) {
	cutoff := c.now().UTC().Add(-CheckInterval)

	var (
		mu     sync.Mutex
		report Report
	)
	work := make(chan Record)
	var wg sync.WaitGroup
	for range c.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for track := range work {
				outcome := c.check(ctx, track)
				mu.Lock()
				report.Checked++
				switch outcome {
				case outcomeFailed:
					report.Failed++
				case outcomePriced:
					report.Priced++
				case outcomeNotified:
					report.Priced++
					report.Notified++
				}
				mu.Unlock()
			}
		}()
	}

	var pageErr error
	after := ""
	for pageErr == nil && ctx.Err() == nil {
		page, err := c.store.Due(ctx, cutoff, after, checkPage)
		if err != nil {
			pageErr = err
			break
		}
		for _, track := range page {
			work <- track
		}
		if len(page) < checkPage {
			break
		}
		after = page[len(page)-1].ID
	}
	close(work)
	wg.Wait()

	if pageErr != nil {
		return report, pageErr
	}
	return report, ctx.Err()
}

type outcome int

const (
	outcomeUnpriced outcome = iota
	outcomePriced
	outcomeNotified
	outcomeFailed
)

// check mirrors the Python actor: read the page, record the check whatever it
// found, and push once when the price is at or below the target.
func (c *Checker) check(ctx context.Context, track Record) outcome {
	quote, err := c.fetcher.FetchPrice(ctx, track.URL)
	if err != nil {
		c.logger.Info("reading a product page failed", "price_track_id", track.ID, "error", err)
		quote = Quote{}
	}

	shouldNotify := quote.Price != nil && *quote.Price <= track.TargetPrice && track.NotifiedAt == nil
	if err := c.store.RecordCheck(ctx, track.ID, Check{
		CheckedAt: c.now().UTC(),
		LastPrice: quote.Price,
		Notified:  shouldNotify,
	}); err != nil {
		c.logger.Error("recording a price check failed", "price_track_id", track.ID, "error", err)
		return outcomeFailed
	}
	if quote.Price == nil {
		return outcomeUnpriced
	}
	if !shouldNotify {
		return outcomePriced
	}

	_, err = c.notifier.SendToUser(ctx, PriceDrop(track, *quote.Price, quote.Currency))
	if err != nil && !errors.Is(err, notify.ErrNoDeviceTokens) {
		c.logger.Error("sending a price drop failed", "price_track_id", track.ID, "error", err)
		return outcomeFailed
	}
	return outcomeNotified
}

// PriceDrop is the push a price at or below target becomes, worded and routed
// as the Python worker sent it.
func PriceDrop(track Record, price float64, currency string) notify.Notification {
	reelID := ""
	if track.SaveID != nil {
		reelID = *track.SaveID
	}
	target, targetID := "price_track", track.ID
	if reelID != "" {
		target, targetID = "reel", reelID
	}
	label := track.Title
	if label == "" {
		label = "A product you're tracking"
	}
	symbol := currency
	if symbol == "" {
		symbol = track.Currency
	}
	return notify.Notification{
		UserID:   track.UserID,
		Title:    "Price drop",
		Body:     label + " is now " + symbol + shortNumber(price) + " (your target: " + symbol + shortNumber(track.TargetPrice) + ").",
		Type:     "price_drop",
		Target:   target,
		TargetID: targetID,
		EventKey: "price_drop:" + track.ID,
		Data:     map[string]string{"price_track_id": track.ID, "reel_id": reelID},
	}
}

// shortNumber is Python's "{:g}": six significant digits, no trailing zeros.
func shortNumber(value float64) string {
	return strconv.FormatFloat(value, 'g', 6, 64)
}
