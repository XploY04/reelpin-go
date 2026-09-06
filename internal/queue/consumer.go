package queue

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// OutcomeKind is what a handler decided about a delivery.
type OutcomeKind int

const (
	// Done: the durable effect is committed, or an earlier delivery of the
	// same message already committed it. Acknowledge.
	Done OutcomeKind = iota
	// Retry: a transport-level failure before any durable state could be
	// written. The message parks in this class's backoff queue. A failure the
	// database recorded schedules its retry through the outbox instead, and
	// the handler answers Done here.
	Retry
	// DeadLetter: poison. A malformed envelope, an unknown schema version, or
	// a failure that prevents the durable state update itself. Rejected
	// without requeue so the broker routes it to this class's dead letters.
	DeadLetter
)

// Outcome carries the decision and, for Retry, which attempt this was, taken
// from database stage state. Broker delivery headers never decide anything.
type Outcome struct {
	Kind    OutcomeKind
	Attempt int
}

// Handler processes one message. It must be safe to call twice with the same
// message: the broker guarantees at-least-once, not exactly-once.
type Handler func(ctx context.Context, message Message) (Outcome, error)

type ConsumerConfig struct {
	// Queue is one workload class. Each class gets its own AMQP channel and
	// its own prefetch, so a busy media channel cannot consume the light
	// queue's credit.
	Queue string
	// Prefetch is the channel QoS. It starts at one message per channel and is
	// raised only with load evidence.
	Prefetch    int
	Logger      *slog.Logger
	ConsumerTag string
}

// Consume runs until ctx is cancelled, reconnecting its channel with bounded
// exponential backoff and jitter, and redeclaring topology after every
// reconnect. It returns only when the context ends or the connection itself is
// closed for good.
func Consume(ctx context.Context, connection *amqp.Connection, config ConsumerConfig, publisher *Publisher, handle Handler) error {
	if config.Prefetch <= 0 {
		config.Prefetch = 1
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		err := consumeOnce(ctx, connection, config, publisher, handle)
		if err == nil {
			return nil // clean shutdown
		}
		if connection.IsClosed() {
			return fmt.Errorf("consuming %s: the connection is gone: %w", config.Queue, err)
		}

		// The channel died but the connection lives: reopen with jittered
		// backoff so a broker restart is not greeted by a stampede.
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)))
		config.Logger.Warn("consumer channel lost, reconnecting",
			"queue", config.Queue, "wait", wait.String(), "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func consumeOnce(ctx context.Context, connection *amqp.Connection, config ConsumerConfig, publisher *Publisher, handle Handler) error {
	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("opening a channel: %w", err)
	}
	defer channel.Close()

	if err := Declare(channel); err != nil {
		return err
	}
	if err := channel.Qos(config.Prefetch, 0, false); err != nil {
		return fmt.Errorf("setting prefetch: %w", err)
	}

	deliveries, err := channel.Consume(config.Queue, config.ConsumerTag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consuming %s: %w", config.Queue, err)
	}

	closed := channel.NotifyClose(make(chan *amqp.Error, 1))
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	for {
		select {
		case <-ctx.Done():
			// Stop taking deliveries; in-flight work finishes under the
			// caller's drain budget.
			return nil
		case reason := <-closed:
			return fmt.Errorf("channel closed: %v", reason)
		case delivery, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("the delivery stream ended")
			}
			inFlight.Add(1)
			func() {
				defer inFlight.Done()
				handleDelivery(ctx, config, publisher, handle, delivery)
			}()
		}
	}
}

func handleDelivery(ctx context.Context, config ConsumerConfig, publisher *Publisher, handle Handler, delivery amqp.Delivery) {
	message, err := Decode(delivery.Body)
	if err != nil {
		// An undecodable or unknown-version message will never decode better.
		// Requeueing it would spin; it goes to this class's dead letters.
		config.Logger.Error("dead-lettering an undecodable message",
			"queue", config.Queue, "error", err)
		_ = delivery.Nack(false, false)
		return
	}

	logger := config.Logger.With(
		"queue", config.Queue,
		"event_id", message.EventID,
		"run_id", message.RunID,
	)

	// Work keeps its own budget: shutdown cancels the loop, not the delivery
	// that is already running.
	outcome, err := handle(context.WithoutCancel(ctx), message)
	switch outcome.Kind {
	case Done:
		if err != nil {
			logger.Error("handler reported done with an error", "error", err)
		}
		if ackErr := delivery.Ack(false); ackErr != nil {
			logger.Error("acknowledging failed", "error", ackErr)
		}

	case Retry:
		logger.Warn("parking a transport retry", "attempt", outcome.Attempt, "error", err)
		if publishErr := publisher.PublishRetry(context.WithoutCancel(ctx), config.Queue, outcome.Attempt, message); publishErr != nil {
			// The retry could not be parked, so the message must stay on the
			// broker: reject it back onto the queue.
			logger.Error("parking the retry failed, requeueing", "error", publishErr)
			_ = delivery.Nack(false, true)
			return
		}
		_ = delivery.Ack(false)

	case DeadLetter:
		logger.Error("dead-lettering", "error", err)
		_ = delivery.Nack(false, false)
	}
}

// DrainTimeout is how long a worker waits for in-flight work at shutdown.
const DrainTimeout = 30 * time.Second
