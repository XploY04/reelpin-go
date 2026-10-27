package spend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// ErrLimitReached means new provider-costing work of this shape is refused
// because the month's spending has reached the point the owner approved. It is
// not transient: only an operator or the next month clears it.
var ErrLimitReached = errors.New("the monthly provider spend limit has been reached")

// GroupAll is the stop-order entry that matches every submission.
const GroupAll = "all"

// The two cost shapes a submission can have. A media run downloads and
// transcribes, which is where the large token counts come from; a light run
// reads a page. They are what a stop order names when it does not name one
// platform.
const (
	ClassLight = "light"
	ClassMedia = "media"
)

// Work is the cost shape of one submission: where the link came from, and which
// queue class it will run on. A stop-order entry matches either, so the owner
// can name a platform ("instagram") or a class ("media").
type Work struct {
	Platform string
	// Class is "media" or "light": whether the run downloads and transcribes.
	Class string
}

// Limits are the three values the product owner approves. There are no
// defaults; a limit this service invented would look like a decision.
type Limits struct {
	WarnMicros Micros
	StopMicros Micros
	// StopOrder is which groups stop taking new work first. Groups are shed in
	// order, spread evenly from the warning amount up to the hard stop, so the
	// last entry stops exactly at the hard stop.
	StopOrder []string
}

// NewLimits validates the approved values together. Two amounts without an
// order, or an order naming the same group twice, are a configuration mistake
// that would otherwise only show up as spending nobody stopped.
func NewLimits(warnUSD, stopUSD, stopOrder string) (Limits, error) {
	warn, err := ParseUSD(warnUSD)
	if err != nil {
		return Limits{}, fmt.Errorf("the warning amount: %w", err)
	}
	stop, err := ParseUSD(stopUSD)
	if err != nil {
		return Limits{}, fmt.Errorf("the hard-stop amount: %w", err)
	}
	if warn == 0 {
		return Limits{}, errors.New("the warning amount is zero, so nothing would ever warn")
	}
	if stop <= warn {
		return Limits{}, fmt.Errorf("the hard stop (%s) must be above the warning amount (%s)",
			stopUSD, warnUSD)
	}

	groups := []string{}
	seen := map[string]bool{}
	for _, group := range strings.Split(stopOrder, ",") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if seen[group] {
			return Limits{}, fmt.Errorf("the stop order names %q twice", group)
		}
		seen[group] = true
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return Limits{}, errors.New("the stop order is empty, so nothing would ever stop")
	}
	if !seen[GroupAll] {
		return Limits{}, fmt.Errorf("the stop order must end with %q, or work nobody named would keep spending past the hard stop", GroupAll)
	}
	if groups[len(groups)-1] != GroupAll {
		return Limits{}, fmt.Errorf("%q must be last in the stop order: it matches everything", GroupAll)
	}
	return Limits{WarnMicros: warn, StopMicros: stop, StopOrder: groups}, nil
}

// ShedAt is the month-to-date spend at which the group at this position stops
// taking new work. Exported because the runbook and the operator both need to
// read the ladder the three approved values imply.
func (l Limits) ShedAt(position int) Micros {
	span := int64(l.StopMicros - l.WarnMicros)
	return l.WarnMicros + Micros(span*int64(position+1)/int64(len(l.StopOrder)))
}

// stopped reports whether this work is refused at this spend.
func (l Limits) stopped(spent Micros, work Work) bool {
	// At or past the hard stop nothing provider-costing is accepted, whatever
	// the order says.
	if spent >= l.StopMicros {
		return true
	}
	for position, group := range l.StopOrder {
		if !matches(group, work) {
			continue
		}
		if spent >= l.ShedAt(position) {
			return true
		}
	}
	return false
}

func matches(group string, work Work) bool {
	return group == GroupAll || group == work.Platform || group == work.Class
}

// MonthToDate is the half of Store the gate needs.
type MonthToDate interface {
	MonthToDateMicros(ctx context.Context) (Micros, error)
}

// Gate refuses new provider-costing submissions once the month's spend reaches
// the approved ladder. It never touches reads, and it never touches work that
// is already committed: a job that has a run has already been paid for, and
// stopping it mid-flight would spend the money and throw the result away.
type Gate struct {
	limits Limits
	spent  MonthToDate
	meters *metrics.Metrics
}

func NewGate(limits Limits, spent MonthToDate, meters *metrics.Metrics) *Gate {
	if meters != nil {
		meters.CostGateWarnUSD.Set(limits.WarnMicros.USD())
		meters.CostGateStopUSD.Set(limits.StopMicros.USD())
	}
	return &Gate{limits: limits, spent: spent, meters: meters}
}

// Allow answers nil when this submission may be accepted, ErrLimitReached when
// it may not, and a plain error when the month's spend could not be read. That
// last case fails closed: an unreadable ledger is an unmetered one.
func (g *Gate) Allow(ctx context.Context, work Work) error {
	spent, err := g.spent.MonthToDateMicros(ctx)
	if err != nil {
		return fmt.Errorf("reading month-to-date provider spend: %w", err)
	}
	if !g.limits.stopped(spent, work) {
		return nil
	}
	if g.meters != nil {
		g.meters.SubmissionsBlocked.WithLabelValues(g.blockedBy(spent, work)).Inc()
	}
	return ErrLimitReached
}

// blockedBy names the stop-order entry that refused this work, so the counter
// says which group is shedding rather than only that something is.
func (g *Gate) blockedBy(spent Micros, work Work) string {
	for position, group := range g.limits.StopOrder {
		if matches(group, work) && spent >= g.limits.ShedAt(position) {
			return group
		}
	}
	return GroupAll
}
