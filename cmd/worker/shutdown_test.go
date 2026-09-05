package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestASignalIsANormalShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	cancel()
	err, componentDied := awaitShutdown(ctx, done)
	if err != nil || componentDied {
		t.Fatalf("err = %v, componentDied = %v, want a clean stop", err, componentDied)
	}
}

func TestAFailedComponentStopsTheWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failure := errors.New("the consumer lost its connection")
	done := make(chan error, 1)
	done <- failure

	err, componentDied := awaitShutdown(ctx, done)
	if !componentDied {
		t.Fatal("a dead consumer did not stop the worker: it would keep heartbeating as healthy")
	}
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the component's error", err)
	}
}

func TestACleanEarlyExitIsStillFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	done <- nil

	err, componentDied := awaitShutdown(ctx, done)
	if !componentDied {
		t.Fatal("a component returning nil before shutdown was treated as normal")
	}
	if err == nil {
		t.Fatal("exiting zero would let a supervisor treat this as a normal stop")
	}
}

func TestAHealthyWorkerKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Nothing on done, no signal: awaitShutdown must block, not return.
	start := time.Now()
	if _, componentDied := awaitShutdown(ctx, make(chan error)); componentDied {
		t.Fatal("a running worker was reported as failed")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("awaitShutdown returned before anything happened")
	}
}
