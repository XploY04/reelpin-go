// Command load drives a running API and reports what it did. It is a
// measurement tool, not a test: it needs a real base URL and a real token, and
// it never runs in CI.
//
//	load --base-url http://localhost:8000 --token "$JWT" --duration 60s --workers 20
//	load --scenario enqueue --rate 5 --duration 30s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	baseURL := flags.String("base-url", "http://localhost:8000", "the API to drive")
	token := flags.String("token", "", "a Supabase access token; required for every scenario but health")
	scenario := flags.String("scenario", "mixed", "health, reads, enqueue, poll, search or mixed")
	duration := flags.Duration("duration", 30*time.Second, "how long to keep sending")
	workers := flags.Int("workers", 10, "concurrent senders")
	rate := flags.Float64("rate", 0, "requests per second per worker; 0 means as fast as the API answers")
	shareURL := flags.String("share-url", "", "the URL the enqueue scenario shares; required for that scenario")
	timeout := flags.Duration("timeout", 30*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	plan, err := planFor(*scenario, *token, *shareURL)
	if err != nil {
		return err
	}
	if *workers < 1 {
		return fmt.Errorf("--workers must be at least 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	driver := &Driver{
		BaseURL: strings.TrimRight(*baseURL, "/"),
		Token:   *token,
		Timeout: *timeout,
		Workers: *workers,
		Rate:    *rate,
	}

	fmt.Printf("driving %s: scenario %s, %d workers, %s\n", driver.BaseURL, *scenario, *workers, *duration)
	report := driver.Run(ctx, plan)
	report.Print(os.Stdout)

	if report.Requests == 0 {
		return fmt.Errorf("no request completed")
	}
	return nil
}
