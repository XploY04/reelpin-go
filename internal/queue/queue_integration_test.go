//go:build integration

package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func testConnection(t *testing.T) *amqp.Connection {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL is not set")
	}
	connection, err := amqp.Dial(url)
	if err != nil {
		t.Skipf("rabbitmq is not reachable: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

// drain empties a queue so one test never inherits another's messages.
func drain(t *testing.T, connection *amqp.Connection, queues ...string) {
	t.Helper()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("opening a channel: %v", err)
	}
	defer channel.Close()
	if err := Declare(channel); err != nil {
		t.Fatalf("declaring: %v", err)
	}
	for _, name := range queues {
		if _, err := channel.QueuePurge(name, false); err != nil {
			t.Fatalf("purging %s: %v", name, err)
		}
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func message(id string) Message {
	return Message{
		EventID:       id,
		RunID:         "11111111-1111-4111-8111-111111111111",
		Platform:      "instagram",
		SchemaVersion: SchemaVersion,
		Type:          "content.process",
	}
}

func TestTopologyIsIdempotent(t *testing.T) {
	connection := testConnection(t)

	for pass := 0; pass < 2; pass++ {
		channel, err := connection.Channel()
		if err != nil {
			t.Fatalf("opening a channel: %v", err)
		}
		if err := Declare(channel); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		channel.Close()
	}
}

func TestPublishIsConfirmedAndDelivered(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueueWeb)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueueWeb, message("22222222-2222-4222-8222-222222222222")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan Message, 1)
	go func() {
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueueWeb, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-delivered",
		}, publisher, func(_ context.Context, m Message) (Outcome, error) {
			received <- m
			return Done, nil
		})
	}()

	select {
	case got := <-received:
		if got.EventID != "22222222-2222-4222-8222-222222222222" {
			t.Fatalf("received %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("the message was never delivered")
	}
}

func TestUnroutablePublishIsAnError(t *testing.T) {
	connection := testConnection(t)
	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	// Mandatory publishing is what keeps a typo from silently dropping work.
	err = publisher.Publish(context.Background(), "reelpin.jobs.nowhere", message("33333333-3333-4333-8333-333333333333"))
	if !errors.Is(err, ErrUnroutable) {
		t.Fatalf("err = %v, want ErrUnroutable", err)
	}
}

func TestUnacknowledgedWorkIsRedelivered(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueuePersonalize)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueuePersonalize, message("44444444-4444-4444-8444-444444444444")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A consumer that dies before acknowledging: the channel closes with the
	// message still unacknowledged, so the broker gives it to the next consumer.
	crashed, err := connection.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := crashed.Qos(1, 0, false); err != nil {
		t.Fatalf("qos: %v", err)
	}
	deliveries, err := crashed.Consume(QueuePersonalize, "test-crash", false, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	select {
	case <-deliveries:
	case <-time.After(10 * time.Second):
		t.Fatal("the first consumer never received the message")
	}
	crashed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	redelivered := make(chan Message, 1)
	go func() {
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueuePersonalize, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-redeliver",
		}, publisher, func(_ context.Context, m Message) (Outcome, error) {
			redelivered <- m
			return Done, nil
		})
	}()

	select {
	case got := <-redelivered:
		if got.EventID != "44444444-4444-4444-8444-444444444444" {
			t.Fatalf("redelivered %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("work left unacknowledged was not redelivered")
	}
}

func TestRetryGoesThroughTheBackoffQueue(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueueWeb)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueueWeb, message("55555555-5555-4555-8555-555555555555")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var attempts atomic.Int64
	go func() {
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueueWeb, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-retry",
		}, publisher, func(_ context.Context, m Message) (Outcome, error) {
			attempts.Add(1)
			return Retry, errors.New("transient")
		})
	}()

	// The first attempt is handled and parked in the 30 second retry queue, so
	// it must not come back inside this window.
	deadline := time.After(4 * time.Second)
	for {
		select {
		case <-deadline:
			if attempts.Load() != 1 {
				t.Fatalf("handled %d times, want one attempt then a wait", attempts.Load())
			}
			// The message is waiting in the backoff queue, not lost.
			channel, err := connection.Channel()
			if err != nil {
				t.Fatalf("channel: %v", err)
			}
			defer channel.Close()
			queueState, err := channel.QueueInspect("reelpin.retry.30s")
			if err != nil {
				t.Fatalf("inspecting the retry queue: %v", err)
			}
			if queueState.Messages == 0 {
				t.Fatal("the retried message is not waiting in the backoff queue")
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestGivingUpDeadLetters(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueueTikTok, DeadLetterQueue(QueueTikTok))

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueueTikTok, message("66666666-6666-4666-8666-666666666666")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handled := make(chan struct{}, 1)
	go func() {
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueueTikTok, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-dead",
		}, publisher, func(_ context.Context, m Message) (Outcome, error) {
			handled <- struct{}{}
			return DeadLetter, errors.New("this will never work")
		})
	}()

	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("the message was never handled")
	}

	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer channel.Close()

	dead := DeadLetterQueue(QueueTikTok)
	for waited := time.Duration(0); waited < 5*time.Second; waited += 200 * time.Millisecond {
		state, err := channel.QueueInspect(dead)
		if err != nil {
			t.Fatalf("inspecting %s: %v", dead, err)
		}
		if state.Messages == 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the message never reached %s", dead)
}

func TestUndecodableMessageIsRejectedNotRequeued(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueueReddit, DeadLetterQueue(QueueReddit))

	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := Declare(channel); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := channel.PublishWithContext(context.Background(), Exchange, QueueReddit, false, false,
		amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: []byte("{not json")},
	); err != nil {
		t.Fatalf("publishing garbage: %v", err)
	}
	channel.Close()

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueueReddit, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-garbage",
		}, publisher, func(context.Context, Message) (Outcome, error) {
			t.Error("the handler ran for an undecodable message")
			return Done, nil
		})
	}()

	inspect, err := connection.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer inspect.Close()

	dead := DeadLetterQueue(QueueReddit)
	for waited := time.Duration(0); waited < 5*time.Second; waited += 200 * time.Millisecond {
		state, err := inspect.QueueInspect(dead)
		if err == nil && state.Messages == 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("an undecodable message did not reach the dead letter queue")
}

func TestShutdownFinishesInFlightWork(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection, QueueLinkedIn)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueueLinkedIn, message("77777777-7777-4777-8777-777777777777")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var finished atomic.Bool
	var wait sync.WaitGroup
	wait.Add(1)

	go func() {
		defer wait.Done()
		_ = Consume(ctx, connection, ConsumerConfig{
			Queue: QueueLinkedIn, Concurrency: 1, Logger: quiet(), ConsumerTag: "test-shutdown",
		}, publisher, func(work context.Context, m Message) (Outcome, error) {
			close(started)
			// Shutdown must not cancel work that is already running.
			time.Sleep(500 * time.Millisecond)
			if work.Err() != nil {
				t.Error("the work context was cancelled by shutdown")
			}
			finished.Store(true)
			return Done, nil
		})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never started")
	}

	cancel()
	done := make(chan struct{})
	go func() { wait.Wait(); close(done) }()

	select {
	case <-done:
		if !finished.Load() {
			t.Fatal("shutdown returned before the running handler finished")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not return")
	}
}
