package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"
)

// runHealthcheck is the container's HEALTHCHECK. It calls liveness, which never
// touches the database, so a failure means this process is gone rather than a
// dependency being slow. It prints nothing on success: Docker keeps the output.
func runHealthcheck(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := flags.String("url", "http://127.0.0.1:8000/api/v1/health/live", "the liveness endpoint to call")
	timeout := flags.Duration("timeout", 3*time.Second, "how long to wait")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("liveness: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("liveness returned %d", resp.StatusCode)
	}
	return nil
}
