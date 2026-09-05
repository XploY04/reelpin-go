package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrUnroutable means the broker accepted the connection but no queue would
// have taken the message. Losing it silently is how work disappears, so it is
// an error the dispatcher retries.
var ErrUnroutable = errors.New("message was not routable")

// Publisher publishes persistently, mandatorily, and waits for the broker's
// confirm. A publish that is not confirmed is reported as failed, so the outbox
// row stays unpublished and is retried.
type Publisher struct {
	mu      sync.Mutex
	channel *amqp.Channel
	returns chan amqp.Return
	timeout time.Duration
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

// Publish sends one message to a queue by name and waits for its confirm.
func (p *Publisher) Publish(ctx context.Context, queue string, message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Drain any return left by an earlier publish so it is not mistaken for
	// this one's.
	p.drainReturns()

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(ctx,
		Exchange, queue,
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

// PublishRetry sends a message into the backoff stage for its attempt. The
// retry queue has no consumer: its TTL expires and the broker dead-letters the
// message back onto the work exchange with the original routing key.
func (p *Publisher) PublishRetry(ctx context.Context, workQueue string, message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.drainReturns()

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(ctx,
		RetryExchange, RetryRoutingKey(message.Attempt),
		true, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    message.EventID,
			Timestamp:    time.Now().UTC(),
			// The work queue is the routing key the broker uses when the delay
			// expires.
			ReplyTo: workQueue,
			Headers: amqp.Table{"x-original-routing-key": workQueue},
			Body:    body,
		})
	if err != nil {
		return fmt.Errorf("publishing a retry: %w", err)
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("waiting for the retry confirm: %w", err)
	}
	if !acked {
		return fmt.Errorf("the broker rejected the retry")
	}
	return nil
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
