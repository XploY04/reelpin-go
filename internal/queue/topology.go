// Package queue owns the RabbitMQ topology and the two sides of it the service
// uses: a confirming publisher and a manual-ack consumer.
//
// Delivery is at-least-once. Every consumer must therefore be safe to run twice
// on the same message; the database, not the broker, decides what has already
// happened.
package queue

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// Exchange is the one topic exchange all work flows through.
	Exchange = "reelpin.jobs"
	// RetryExchange holds messages that are waiting out a backoff.
	RetryExchange = "reelpin.retry"
	// DeadLetterExchange holds messages that gave up.
	DeadLetterExchange = "reelpin.dead"
)

// Work queues, one per provider boundary plus the cheap ones. A slow or rate
// limited provider must not stall another platform's queue.
const (
	QueueInstagram   = "reelpin.jobs.instagram"
	QueueYouTube     = "reelpin.jobs.youtube"
	QueueTikTok      = "reelpin.jobs.tiktok"
	QueueLinkedIn    = "reelpin.jobs.linkedin"
	QueueReddit      = "reelpin.jobs.reddit"
	QueueWeb         = "reelpin.jobs.web"
	QueuePersonalize = "reelpin.jobs.personalize"
	QueueNotify      = "reelpin.notifications"
)

// WorkQueues is every queue a worker consumes, with the routing key that feeds
// it. The key is the queue name, so a publisher only needs the queue.
var WorkQueues = []string{
	QueueInstagram,
	QueueYouTube,
	QueueTikTok,
	QueueLinkedIn,
	QueueReddit,
	QueueWeb,
	QueuePersonalize,
	QueueNotify,
}

// RetryDelays are the three backoff stages. A message sits in the matching
// retry queue with no consumer and is dead-lettered back onto its work queue
// when the TTL expires.
var RetryDelays = []time.Duration{
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
}

func retryQueue(delay time.Duration) string {
	return fmt.Sprintf("reelpin.retry.%ds", int(delay.Seconds()))
}

// DeadLetterQueue is where a queue's give-ups land.
func DeadLetterQueue(workQueue string) string {
	return workQueue + ".dead"
}

// Declare builds the whole topology. It is idempotent, so every process may
// call it on connect.
func Declare(channel *amqp.Channel) error {
	for _, exchange := range []string{Exchange, RetryExchange, DeadLetterExchange} {
		if err := channel.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
			return fmt.Errorf("declaring exchange %s: %w", exchange, err)
		}
	}

	for _, queue := range WorkQueues {
		dead := DeadLetterQueue(queue)
		if _, err := channel.QueueDeclare(dead, true, false, false, false, amqp.Table{
			"x-queue-type": "quorum",
		}); err != nil {
			return fmt.Errorf("declaring dead letter queue %s: %w", dead, err)
		}
		if err := channel.QueueBind(dead, dead, DeadLetterExchange, false, nil); err != nil {
			return fmt.Errorf("binding dead letter queue %s: %w", dead, err)
		}

		// A give-up is routed to this queue's own dead letter queue, so one
		// platform's failures never mix into another's.
		if _, err := channel.QueueDeclare(queue, true, false, false, false, amqp.Table{
			"x-queue-type":              "quorum",
			"x-dead-letter-exchange":    DeadLetterExchange,
			"x-dead-letter-routing-key": dead,
		}); err != nil {
			return fmt.Errorf("declaring queue %s: %w", queue, err)
		}
		if err := channel.QueueBind(queue, queue, Exchange, false, nil); err != nil {
			return fmt.Errorf("binding queue %s: %w", queue, err)
		}
	}

	// Retry queues have no consumer. Their TTL expires and the message is
	// dead-lettered back onto the work exchange with its original routing key.
	for _, delay := range RetryDelays {
		name := retryQueue(delay)
		if _, err := channel.QueueDeclare(name, true, false, false, false, amqp.Table{
			"x-queue-type":           "quorum",
			"x-message-ttl":          int32(delay.Milliseconds()),
			"x-dead-letter-exchange": Exchange,
		}); err != nil {
			return fmt.Errorf("declaring retry queue %s: %w", name, err)
		}
		if err := channel.QueueBind(name, name, RetryExchange, false, nil); err != nil {
			return fmt.Errorf("binding retry queue %s: %w", name, err)
		}
	}
	return nil
}

// RetryRoutingKey picks the backoff stage for an attempt. Attempts past the
// last stage stay at the longest one; giving up is the caller's decision.
func RetryRoutingKey(attempt int) string {
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(RetryDelays) {
		index = len(RetryDelays) - 1
	}
	return retryQueue(RetryDelays[index])
}
