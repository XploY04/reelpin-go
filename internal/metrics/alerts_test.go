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
	m.ObserveRequest("/api/v1/reels", "GET", 200, 1)
	m.ObserveSearch("dense", 1, 1)
	m.ObserveStage("save", "instagram", "ok", 1)
	m.DatabasePool.WithLabelValues("acquired").Set(1)
	m.QueuePublished.WithLabelValues("q", "confirmed").Inc()
	m.QueueConsumed.WithLabelValues("q", "done").Inc()
	m.QueueRetried.WithLabelValues("q").Inc()
	m.DeadLettered.WithLabelValues("q").Inc()
	m.ProviderFailures.WithLabelValues("instagram", "transient").Inc()
	m.ProviderCooldown.WithLabelValues("instagram").Set(1)
	m.PushDelivery.WithLabelValues("sent").Inc()
	m.CacheEvents.WithLabelValues("filters", "hit").Inc()
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
		available := map[string]bool{"le": true}
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

func TestPoolAlertDoesNotCompareMismatchedSeries(t *testing.T) {
	// Prometheus matches on all labels by default, so comparing two series that
	// differ only by `state` never matches and the rule can never fire.
	rules := alertsFile(t)
	if strings.Contains(rules, `reelpin_db_pool_connections{state="acquired"}\n            >=`) {
		t.Error("the pool comparison still matches on the state label")
	}
	if !strings.Contains(rules, `sum(reelpin_db_pool_connections{state="acquired"})`) {
		t.Error("the pool alert should aggregate the state label away before comparing")
	}
}
