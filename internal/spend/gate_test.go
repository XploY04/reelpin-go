package spend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// The three values are illustrative and belong to the test, not to the service:
// nothing in the code carries an amount.
const (
	testWarn = "12.00"
	testStop = "20.00"
	// Four groups, so each sheds two dollars apart: instagram at $14, media at
	// $16, light at $18, everything at $20.
	testOrder = "instagram, media, light, all"
)

func testLimits(t *testing.T) Limits {
	t.Helper()
	limits, err := NewLimits(testWarn, testStop, testOrder)
	if err != nil {
		t.Fatalf("NewLimits: %v", err)
	}
	return limits
}

// fixedSpend answers one month-to-date figure, or an error.
type fixedSpend struct {
	micros Micros
	err    error
}

func (f fixedSpend) MonthToDateMicros(context.Context) (Micros, error) {
	return f.micros, f.err
}

func TestTheLadderIsExactlyTheApprovedAmounts(t *testing.T) {
	limits := testLimits(t)

	want := []Micros{14_000_000, 16_000_000, 18_000_000, 20_000_000}
	for position, expected := range want {
		if got := limits.ShedAt(position); got != expected {
			t.Errorf("%s sheds at %d micros, want %d",
				limits.StopOrder[position], got, expected)
		}
	}
	// The last group must land on the approved hard stop, to the micro.
	if limits.ShedAt(len(limits.StopOrder)-1) != limits.StopMicros {
		t.Error("the last group does not shed exactly at the hard stop")
	}
}

func TestGroupsStopInTheApprovedOrderAtTheExactValues(t *testing.T) {
	limits := testLimits(t)
	instagram := Work{Platform: "instagram", Class: ClassMedia}
	youtube := Work{Platform: "youtube", Class: ClassMedia}
	blog := Work{Platform: "someblog.com", Class: ClassLight}

	tests := []struct {
		name    string
		spent   Micros
		stopped map[string]bool
	}{
		{"below the warning", 11_999_999, map[string]bool{"instagram": false, "youtube": false, "blog": false}},
		{"at the warning", 12_000_000, map[string]bool{"instagram": false, "youtube": false, "blog": false}},
		{"one micro below the first step", 13_999_999, map[string]bool{"instagram": false, "youtube": false, "blog": false}},
		{"at the first step", 14_000_000, map[string]bool{"instagram": true, "youtube": false, "blog": false}},
		{"at the media step", 16_000_000, map[string]bool{"instagram": true, "youtube": true, "blog": false}},
		{"at the light step", 18_000_000, map[string]bool{"instagram": true, "youtube": true, "blog": true}},
		{"one micro below the hard stop", 19_999_999, map[string]bool{"instagram": true, "youtube": true, "blog": true}},
		{"at the hard stop", 20_000_000, map[string]bool{"instagram": true, "youtube": true, "blog": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, work := range map[string]Work{
				"instagram": instagram, "youtube": youtube, "blog": blog,
			} {
				if got := limits.stopped(tt.spent, work); got != tt.stopped[name] {
					t.Errorf("%s stopped = %v at %d micros, want %v",
						name, got, tt.spent, tt.stopped[name])
				}
			}
		})
	}
}

func TestTheHardStopRefusesEverythingEvenAGroupNobodyNamed(t *testing.T) {
	// A stop order that names no group for a platform must not leave that
	// platform spending past the hard stop.
	limits, err := NewLimits("12.00", "20.00", "instagram,all")
	if err != nil {
		t.Fatalf("NewLimits: %v", err)
	}
	pinterest := Work{Platform: "pinterest", Class: ClassLight}
	if limits.stopped(19_999_999, pinterest) {
		t.Error("pinterest stopped before the hard stop, and nothing named it")
	}
	if !limits.stopped(20_000_000, pinterest) {
		t.Error("pinterest is still accepted at the hard stop")
	}
}

func TestAllowAnswersTheStableSentinelAndCountsTheGroup(t *testing.T) {
	meters := metrics.New()
	gate := NewGate(testLimits(t), fixedSpend{micros: 20_000_000}, meters)

	err := gate.Allow(context.Background(), Work{Platform: "youtube", Class: ClassMedia})
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("err = %v, want ErrLimitReached", err)
	}

	body := exposition(t, meters)
	if !strings.Contains(body, `reelpin_submissions_blocked_total{group="instagram"}`) &&
		!strings.Contains(body, `reelpin_submissions_blocked_total{group="media"}`) {
		t.Errorf("the refusal was not counted against a stop-order group:\n%s", body)
	}
	// The approved amounts are on the registry, so an alert can compare against
	// them rather than repeating them in a rules file.
	for _, want := range []string{
		"reelpin_cost_gate_warn_usd 12", "reelpin_cost_gate_stop_usd 20",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not carry %q", want)
		}
	}
}

func TestAllowFailsClosedWhenTheLedgerCannotBeRead(t *testing.T) {
	// An unreadable ledger is an unmetered one. Refusing is recoverable;
	// spending money nobody can see is not.
	gate := NewGate(testLimits(t), fixedSpend{err: errors.New("no connection")}, nil)
	err := gate.Allow(context.Background(), Work{Platform: "youtube", Class: ClassMedia})
	if err == nil {
		t.Fatal("a submission was accepted while the spend could not be read")
	}
	if errors.Is(err, ErrLimitReached) {
		t.Error("a read failure was reported as the limit, which would tell an operator the wrong thing")
	}
}

func TestNewLimitsRefusesAHalfMadeDecision(t *testing.T) {
	tests := []struct {
		name, warn, stop, order string
	}{
		{"no warning", "0", "20.00", "all"},
		{"stop below warning", "20.00", "12.00", "all"},
		{"stop equal to warning", "12.00", "12.00", "all"},
		{"empty order", "12.00", "20.00", ""},
		{"order without a catch-all", "12.00", "20.00", "instagram,media"},
		{"catch-all not last", "12.00", "20.00", "all,instagram"},
		{"duplicate group", "12.00", "20.00", "media,media,all"},
		{"unparseable amount", "twelve", "20.00", "all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewLimits(tt.warn, tt.stop, tt.order); err == nil {
				t.Error("accepted")
			}
		})
	}
}
