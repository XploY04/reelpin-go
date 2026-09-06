package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrUnroutable means the broker accepted the publish but no queue would have
// taken the message. Losing it silently is how work disappears, so it is an
// error the outbox dispatcher retries.
var ErrUnroutable = errors.New("message was not routable")

// Publisher publishes persistently, mandatorily, and waits for the broker's
// confirm. A publish that is not confirmed is reported as failed, so the outbox
// row stays unpublished and is retried; a duplicate publish after an unknown
// confirm is safe because consumers are idempotent by event id.
type Publisher struct {
	mu      sync.Mutex
	channel *amqp.Channel
	returns chan amqp.Return
	timeout time.Duration
	// Metrics is optional. Nil means the publisher counts nothing.
	Metrics *metrics.Metrics
}

func NewPublisher(connection *amqp.Connection, confirmTimeout time.Duration) (*Publisher, error) {
	channel, err := connection.Channel()
	if err != nil {
		return nil, fmt.Errorf("opening a publish channel: %w", err)
	}
	if err := Declare(channel); err != nil {
		channel.Close()
		return nil, err
	}
	if err := channel.Confirm(false); err != nil {
		channel.Close()
		return nil, fmt.Errorf("enabling publisher confirms: %w", err)
	}
	if confirmTimeout <= 0 {
		confirmTimeout = 5 * time.Second
	}

	return &Publisher{
		channel: channel,
		returns: channel.NotifyReturn(make(chan amqp.Return, 8)),
		timeout: confirmTimeout,
	}, nil
}

// Publish sends one message to a routing key and waits for its confirm. The
// outbox event id travels as MessageId, so a consumer can dedupe and an
// operator can trace a delivery back to its row.
func (p *Publisher) Publish(ctx context.Context, routingKey string, message Message) (err error) {
	defer func() {
		if p.Metrics != nil {
			outcome := "confirmed"
			if err != nil {
				outcome = "failed"
			}
			p.Metrics.QueuePublished.WithLabelValues(routingKey, outcome).Inc()
		}
	}()

	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("message is %d bytes, cap is %d: something other than identifiers got in", len(body), MaxMessageBytes)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Drain any return left by an earlier publish so it is not mistaken for
	// this one's.
	p.drainReturns()

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(ctx,
		Exchange, routingKey,
		true,  // mandatory: an unroutable message must come back, not vanish
		false, // immediate is not supported by modern brokers
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    message.EventID,
			Timestamp:    time.Now().UTC(),
			Body:         body,
		})
	if err != nil {
		return fmt.Errorf("publishing: %w", err)
	}

	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("waiting for the publish confirm: %w", err)
	}
	if !acked {
		return fmt.Errorf("the broker rejected the message")
	}

	// A mandatory publish that reached no queue is returned before the confirm.
	select {
	case returned := <-p.returns:
		return fmt.Errorf("%w: %s (%d)", ErrUnroutable, returned.ReplyText, returned.ReplyCode)
	default:
		return nil
	}
}

// PublishRetry parks a message in its class's backoff queue for the given
// attempt. The parking queue has no consumer: its TTL expires and the broker
// routes the message back to the work queue. This path is for transport
// failures only; a failure that reached durable state schedules its retry
// through the outbox instead, on the database clock.
func (p *Publisher) PublishRetry(ctx context.Context, workQueue string, attempt int, message Message) error {
	err := p.Publish(ctx, RetryRoutingKey(workQueue, attempt), message)
	if err == nil && p.Metrics != nil {
		p.Metrics.QueueRetried.WithLabelValues(workQueue).Inc()
	}
	return err
}

func (p *Publisher) drainReturns() {
	for {
		select {
		case <-p.returns:
		default:
			return
		}
	}
}

func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channel.Close()
}
