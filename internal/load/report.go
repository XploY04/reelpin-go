package load

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

type callStats struct {
	Name      string
	Requests  int
	Errors    int
	Latencies []time.Duration
	Statuses  map[int]int
}

type Report struct {
	Scenario string
	Elapsed  time.Duration
	Requests int
	Errors   int
	// Rejected counts 429s separately: a rate limiter doing its job is not a
	// failure, but it is not a served request either.
	Rejected  int
	Latencies []time.Duration
	Statuses  map[int]int
	byCall    map[string]*callStats
}

func NewReport(scenario string) *Report {
	return &Report{Scenario: scenario, Statuses: map[int]int{}, byCall: map[string]*callStats{}}
}

func (r *Report) add(s sample) {
	r.Requests++
	r.Latencies = append(r.Latencies, s.elapsed)

	stats, ok := r.byCall[s.name]
	if !ok {
		stats = &callStats{Name: s.name, Statuses: map[int]int{}}
		r.byCall[s.name] = stats
	}
	stats.Requests++
	stats.Latencies = append(stats.Latencies, s.elapsed)

	if s.err {
		r.Errors++
		stats.Errors++
		return
	}

	r.Statuses[s.status]++
	stats.Statuses[s.status]++
	if s.status == 429 {
		r.Rejected++
	}
	if s.status >= 500 {
		r.Errors++
		stats.Errors++
	}
}

func (r *Report) Print(w io.Writer) {
	if r.Requests == 0 {
		fmt.Fprintln(w, "no requests completed")
		return
	}

	sorted := sortDurations(r.Latencies)
	throughput := float64(r.Requests) / r.Elapsed.Seconds()

	fmt.Fprintf(w, "\nscenario %s over %s\n", r.Scenario, r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "  requests      %d (%.1f/s)\n", r.Requests, throughput)
	fmt.Fprintf(w, "  errors        %d (%.2f%%)\n", r.Errors, 100*float64(r.Errors)/float64(r.Requests))
	fmt.Fprintf(w, "  rate limited  %d\n", r.Rejected)
	fmt.Fprintf(w, "  latency       p50 %s  p95 %s  p99 %s  max %s\n",
		percentile(sorted, 50).Round(time.Millisecond),
		percentile(sorted, 95).Round(time.Millisecond),
		percentile(sorted, 99).Round(time.Millisecond),
		sorted[len(sorted)-1].Round(time.Millisecond))

	fmt.Fprintln(w, "\n  by status:")
	codes := make([]int, 0, len(r.Statuses))
	for code := range r.Statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	for _, code := range codes {
		fmt.Fprintf(w, "    %d  %d\n", code, r.Statuses[code])
	}

	names := make([]string, 0, len(r.byCall))
	for name := range r.byCall {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(w, "\n  by call:")
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "    call\trequests\terrors\tp50\tp95")
	for _, name := range names {
		stats := r.byCall[name]
		callSorted := sortDurations(stats.Latencies)
		fmt.Fprintf(table, "    %s\t%d\t%d\t%s\t%s\n",
			stats.Name, stats.Requests, stats.Errors,
			percentile(callSorted, 50).Round(time.Millisecond),
			percentile(callSorted, 95).Round(time.Millisecond))
	}
	table.Flush()
}
