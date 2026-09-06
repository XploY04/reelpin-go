package metrics

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// An alert that names a metric nobody exports, or a label nobody sets, is an
// alert that silently never fires. These are the two mistakes a rules file
// cannot catch on its own, so they are checked here against the real registry.
const alertsPath = "../../deploy/alerts.yml"

func alertsFile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(alertsPath)
	if err != nil {
		t.Fatalf("reading the rules: %v", err)
	}
	return string(raw)
}

// exported maps each series to the labels it actually carries, with every
// collector touched so nothing is missing merely because it has never been
// observed.
func exported(t *testing.T) map[string]map[string]bool {
	t.Helper()
	m := New()
	m.ObserveRequest("/api/v2/reels", "GET", 200, 1)
	m.ObserveStage("persist", "instagram", "ok", 1)
	m.DatabasePool.WithLabelValues("acquired").Set(1)
	m.QueuePublished.WithLabelValues("reelpin.processing.light", "confirmed").Inc()
	m.QueueConsumed.WithLabelValues("q", "done").Inc()
	m.QueueRetried.WithLabelValues("q").Inc()
	m.DeadLettered.WithLabelValues("q").Inc()
	m.ProviderFailures.WithLabelValues("instagram", "transient").Inc()
	m.ObserveSearch("hybrid", 0, 1)
	m.PushDelivery.WithLabelValues("sent").Inc()
	m.ProviderCalls.WithLabelValues("gemini", "gemini-2.0-flash-lite", "extract", "measured").Inc()
	m.ProviderTokens.WithLabelValues("gemini", "gemini-2.0-flash-lite", "extract", "input").Inc()
	m.SubmissionsBlocked.WithLabelValues("media").Inc()
	m.ProviderSpendMonthUSD.Set(1)
	m.CostGateWarnUSD.Set(1)
	m.CostGateStopUSD.Set(1)
	m.OutboxAgeSeconds.Set(1)
	m.OldestQueuedJobAge.Set(1)
	m.TempDiskBytes.Set(1)
	m.LiveWorkers.Set(1)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	byName := map[string]map[string]bool{}
	for _, family := range families {
		labels := map[string]bool{}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = true
			}
		}
		byName[family.GetName()] = labels
	}
	return byName
}

// alertBlocks splits the rules file into one chunk per alert, so a label can be
// checked against the metrics that alert actually queries.
func alertBlocks(t *testing.T) map[string]string {
	t.Helper()
	blocks := map[string]string{}
	chunks := strings.Split(alertsFile(t), "- alert:")
	for _, chunk := range chunks[1:] {
		name := strings.TrimSpace(strings.SplitN(chunk, "\n", 2)[0])
		blocks[name] = chunk
	}
	if len(blocks) == 0 {
		t.Fatal("no alerts found in the rules file")
	}
	return blocks
}

func TestEveryAlertNamesAMetricThatExists(t *testing.T) {
	known := exported(t)
	referenced := regexp.MustCompile(`reelpin_[a-z_]+`).FindAllString(alertsFile(t), -1)
	if len(referenced) == 0 {
		t.Fatal("the rules file references no metric at all")
	}

	missing := map[string]bool{}
	for _, name := range referenced {
		// Histograms are queried through their generated series.
		base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
		if _, ok := known[name]; ok {
			continue
		}
		if _, ok := known[base]; ok {
			continue
		}
		missing[name] = true
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Fatalf("alerts reference metrics nothing exports: %v", names)
	}
}

func TestEveryAlertLabelBelongsToThatAlertsOwnMetric(t *testing.T) {
	// A label that exists on some other metric is the trap here: it looks
	// right, and renders as an empty string in the alert a person has to read.
	known := exported(t)
	metricName := regexp.MustCompile(`reelpin_[a-z_]+`)
	labelUse := regexp.MustCompile(`\$labels\.([a-z_]+)`)

	for alert, block := range alertBlocks(t) {
		// le comes from the histogram, instance from the scrape itself.
		available := map[string]bool{"le": true, "instance": true}
		for _, name := range metricName.FindAllString(block, -1) {
			base := strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
			for _, candidate := range []string{name, base} {
				for label := range known[candidate] {
					available[label] = true
				}
			}
		}

		for _, match := range labelUse.FindAllStringSubmatch(block, -1) {
			if !available[match[1]] {
				t.Errorf("alert %s uses $labels.%s, which none of its own metrics carry", alert, match[1])
			}
		}
	}
}

func TestCostAlertsCompareAgainstTheConfiguredLimits(t *testing.T) {
	// The amounts are a product decision the owner approves in the environment.
	// A number written into a rules file is a second limit, and the two drift.
	blocks := alertBlocks(t)
	for alert, gauge := range map[string]string{
		"ProviderSpendWarning":  "reelpin_cost_gate_warn_usd",
		"ProviderSpendHardStop": "reelpin_cost_gate_stop_usd",
	} {
		block, ok := blocks[alert]
		if !ok {
			t.Fatalf("alert %s is missing from the rules", alert)
		}
		if !strings.Contains(block, "reelpin_provider_spend_month_usd >= "+gauge) {
			t.Errorf("alert %s does not compare month-to-date spend against %s", alert, gauge)
		}
	}
}

func TestPoolAlertDoesNotCompareMismatchedSeries(t *testing.T) {
	// Prometheus matches on all labels by default, so comparing two series that
	// differ only by `state` never matches and the rule can never fire.
	if !strings.Contains(alertsFile(t),
		`sum by (instance) (reelpin_db_pool_connections{state="acquired"})`) {
		t.Error("the pool alert should aggregate the state label away before comparing")
	}
}
