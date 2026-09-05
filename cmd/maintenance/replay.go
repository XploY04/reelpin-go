package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/queue"
	amqp "github.com/rabbitmq/amqp091-go"
)

// runReplay moves messages out of a dead letter queue and back onto its work
// queue. It is bounded and manual on purpose: a dead letter is something a
// person decided to look at.
func runReplay(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("replay-dead-letters", flag.ContinueOnError)
	workQueue := flags.String("queue", "", "work queue whose dead letters to replay, for example reelpin.jobs.instagram")
	max := flags.Int("max", 50, "how many messages to move")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *workQueue == "" {
		return errors.New("--queue is required")
	}
	if cfg.RabbitMQURL == "" {
		return errors.New("RABBITMQ_URL is required to replay dead letters")
	}

	connection, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		return fmt.Errorf("rabbitmq connect: %w", err)
	}
	defer connection.Close()

	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("opening a channel: %w", err)
	}
	defer channel.Close()

	publisher, err := queue.NewPublisher(connection, 5*time.Second)
	if err != nil {
		return err
	}
	defer publisher.Close()

	dead := queue.DeadLetterQueue(*workQueue)
	moved, skipped := 0, 0

	for moved+skipped < *max {
		delivery, ok, err := channel.Get(dead, false)
		if err != nil {
			return fmt.Errorf("reading %s: %w", dead, err)
		}
		if !ok {
			break
		}

		message, err := queue.Decode(delivery.Body)
		if err != nil {
			// Leave what cannot be decoded where it is, for a person to see.
			logger.Warn("skipping an undecodable dead letter", "queue", dead, "error", err)
			_ = delivery.Nack(false, true)
			skipped++
			continue
		}

		// The attempt counter restarts, so a replayed message gets the full
		// backoff ladder again rather than dying on its first hiccup.
		message.Attempt = 0
		if err := publisher.Publish(ctx, *workQueue, message); err != nil {
			_ = delivery.Nack(false, true)
			return fmt.Errorf("republishing %s: %w", message.EventID, err)
		}
		if err := delivery.Ack(false); err != nil {
			return fmt.Errorf("acknowledging a replayed message: %w", err)
		}
		moved++
	}

	logger.Info("dead letters replayed", "queue", *workQueue, "moved", moved, "skipped", skipped)
	return nil
}
