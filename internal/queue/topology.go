// Package queue owns the RabbitMQ topology and the two sides of it the service
// uses: a confirming publisher and a manual-ack consumer.
//
// Delivery is at-least-once. Every consumer must be safe to run twice on the
// same message; PostgreSQL, not the broker, decides what has already happened.
package queue

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exchange is the one topic exchange all processing flows through.
const Exchange = "reelpin.processing"

// Two workload classes, so a long download can never starve a cheap page
// fetch of consumer credit. Routing is deterministic from platform and source
// metadata; a model never selects a queue.
const (
	QueueMedia = "reelpin.processing.media"
	QueueLight = "reelpin.processing.light"
	// QueueNotify is its own durable queue with its own retries: a
	// notification failure must never occupy processing credit.
	QueueNotify = "reelpin.notifications"
)

// WorkQueues is every queue a worker consumes.
var WorkQueues = []string{QueueMedia, QueueLight, QueueNotify}

// RetryDelays are the two backoff stages the broker owns. These cover
// transport failures only: a failure that reached durable state schedules its
// retry through the outbox with the database clock, not through a TTL.
var RetryDelays = []time.Duration{30 * time.Second, 5 * time.Minute}

// retryQueue is the parking queue for one class and delay. Each class has its
// own, because a shared retry queue has a single dead-letter routing key and
// would return one class's messages to the other's queue.
func retryQueue(workQueue string, delay time.Duration) string {
	return fmt.Sprintf("%s.retry.%ds", workQueue, int(delay.Seconds()))
}

// DeadLetterQueue is where one class's poison lands: malformed envelopes,
// unknown schema versions, and failures that prevented a durable state update.
// A normal terminal business failure updates the private job and is
// acknowledged; it never comes here.
func DeadLetterQueue(workQueue string) string {
	return workQueue + ".dead"
}

// Declare builds the whole topology. It is idempotent, so every process
// declares on connect and after every reconnect.
func Declare(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(Exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declaring exchange %s: %w", Exchange, err)
	}

	for _, queue := range WorkQueues {
		dead := DeadLetterQueue(queue)
		if _, err := channel.QueueDeclare(dead, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declaring dead letter queue %s: %w", dead, err)
		}
		if err := channel.QueueBind(dead, dead, Exchange, false, nil); err != nil {
			return fmt.Errorf("binding dead letter queue %s: %w", dead, err)
		}

		if _, err := channel.QueueDeclare(queue, true, false, false, false, amqp.Table{
			"x-dead-letter-exchange":    Exchange,
			"x-dead-letter-routing-key": dead,
		}); err != nil {
			return fmt.Errorf("declaring queue %s: %w", queue, err)
		}
		if err := channel.QueueBind(queue, queue, Exchange, false, nil); err != nil {
			return fmt.Errorf("binding queue %s: %w", queue, err)
		}

		// Per-class retry parking. No consumer: the TTL expires and the broker
		// dead-letters the message back onto the exchange with this class's
		// work routing key, so a retry cannot lose its return route or block
		// the other class.
		for _, delay := range RetryDelays {
			name := retryQueue(queue, delay)
			if _, err := channel.QueueDeclare(name, true, false, false, false, amqp.Table{
				"x-message-ttl":             int32(delay.Milliseconds()),
				"x-dead-letter-exchange":    Exchange,
				"x-dead-letter-routing-key": queue,
			}); err != nil {
				return fmt.Errorf("declaring retry queue %s: %w", name, err)
			}
			if err := channel.QueueBind(name, name, Exchange, false, nil); err != nil {
				return fmt.Errorf("binding retry queue %s: %w", name, err)
			}
		}
	}
	return nil
}

// RetryRoutingKey picks the parking queue for one class and attempt. Attempts
// past the last stage stay at the longest delay; giving up is the caller's
// decision, made from database attempt state, never from broker headers.
func RetryRoutingKey(workQueue string, attempt int) string {
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(RetryDelays) {
		index = len(RetryDelays) - 1
	}
	return retryQueue(workQueue, RetryDelays[index])
}
