package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Outcome is what a handler decided about a delivery.
type Outcome int

const (
	// Done: the work is finished, or was already finished by an earlier
	// delivery of the same message. Acknowledge it.
	Done Outcome = iota
	// Retry: a transient failure. The message is republished into the backoff
	// stage for its attempt and acknowledged here.
	Retry
	// DeadLetter: this message will never succeed. Reject it without requeue so
	// the broker routes it to the queue's dead letter queue.
	DeadLetter
)

// Handler processes one message. It must be safe to call twice with the same
// message: the broker guarantees at-least-once, not exactly-once.
type Handler func(ctx context.Context, message Message) (Outcome, error)

type ConsumerConfig struct {
	// Queue is consumed with prefetch equal to Concurrency, so an idle worker
	// never holds messages another worker could be running.
	Queue       string
	Concurrency int
	// Global bounds every queue this process consumes together.
	Global      chan struct{}
	Logger      *slog.Logger
	ConsumerTag string
}

// Consume runs until ctx is cancelled, then stops taking new deliveries and
// waits for the ones in flight.
func Consume(ctx context.Context, connection *amqp.Connection, config ConsumerConfig, publisher *Publisher, handle Handler) error {
	if config.Concurrency <= 0 {
		config.Concurrency = 1
	}

	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("opening a consume channel: %w", err)
	}
	defer channel.Close()

	if err := channel.Qos(config.Concurrency, 0, false); err != nil {
		return fmt.Errorf("setting prefetch: %w", err)
	}

	deliveries, err := channel.Consume(config.Queue, config.ConsumerTag,
		false, // manual acknowledgement: nothing is acknowledged before it is done
		false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consuming %s: %w", config.Queue, err)
	}

	slots := make(chan struct{}, config.Concurrency)
	var inFlight sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			// Stop taking deliveries first, then let the running ones finish.
			if err := channel.Cancel(config.ConsumerTag, false); err != nil {
				config.Logger.Warn("cancelling the consumer failed", "queue", config.Queue, "error", err)
			}
			inFlight.Wait()
			return nil

		case delivery, ok := <-deliveries:
			if !ok {
				inFlight.Wait()
				return errors.New("the delivery channel closed")
			}

			slots <- struct{}{}
			if config.Global != nil {
				config.Global <- struct{}{}
			}
			inFlight.Add(1)

			go func(delivery amqp.Delivery) {
				defer func() {
					<-slots
					if config.Global != nil {
						<-config.Global
					}
					inFlight.Done()
				}()
				handleDelivery(ctx, config, publisher, handle, delivery)
			}(delivery)
		}
	}
}

func handleDelivery(ctx context.Context, config ConsumerConfig, publisher *Publisher, handle Handler, delivery amqp.Delivery) {
	message, err := Decode(delivery.Body)
	if err != nil {
		// An undecodable message will never decode. Requeueing it would spin.
		config.Logger.Error("rejecting an undecodable message", "queue", config.Queue, "error", err)
		_ = delivery.Nack(false, false)
		return
	}

	logger := config.Logger.With(
		"queue", config.Queue,
		"event_id", message.EventID,
		"run_id", message.RunID,
		"attempt", message.Attempt,
	)

	// Work keeps its own budget: shutdown cancels the loop, not the delivery
	// that is already running.
	outcome, err := handle(context.WithoutCancel(ctx), message)
	switch outcome {
	case Done:
		if err != nil {
			logger.Error("handler reported done with an error", "error", err)
		}
		if ackErr := delivery.Ack(false); ackErr != nil {
			logger.Error("acknowledging failed", "error", ackErr)
		}

	case Retry:
		logger.Warn("scheduling a retry", "error", err)
		message.Attempt++
		if publishErr := publisher.PublishRetry(context.WithoutCancel(ctx), config.Queue, message); publishErr != nil {
			// The retry could not be scheduled, so the message must stay on the
			// broker: reject it back onto the queue.
			logger.Error("scheduling the retry failed, requeueing", "error", publishErr)
			_ = delivery.Nack(false, true)
			return
		}
		_ = delivery.Ack(false)

	case DeadLetter:
		logger.Error("dead lettering", "error", err)
		_ = delivery.Nack(false, false)
	}
}

// DrainTimeout is how long a worker waits for in-flight work at shutdown.
const DrainTimeout = 30 * time.Second
