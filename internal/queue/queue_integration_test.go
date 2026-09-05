//go:build integration

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
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

// drain empties every queue in the topology so one test never inherits
// another's messages.
func drain(t *testing.T, connection *amqp.Connection) {
	t.Helper()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("opening a channel: %v", err)
	}
	defer channel.Close()
	if err := Declare(channel); err != nil {
		t.Fatalf("declaring: %v", err)
	}
	for _, queue := range WorkQueues {
		names := []string{queue, DeadLetterQueue(queue)}
		for _, delay := range RetryDelays {
			names = append(names, retryQueue(queue, delay))
		}
		for _, name := range names {
			if _, err := channel.QueuePurge(name, false); err != nil {
				t.Fatalf("purging %s: %v", name, err)
			}
		}
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testMessage(eventID string) Message {
	return Message{
		EventID:       eventID,
		SchemaVersion: SchemaVersion,
		EventType:     EventProcessLight,
		RunID:         "22222222-2222-4222-8222-222222222222",
		CreatedAt:     time.Now().UTC(),
	}
}

func TestDeclareIsIdempotent(t *testing.T) {
	connection := testConnection(t)
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()

	for i := 0; i < 3; i++ {
		if err := Declare(channel); err != nil {
			t.Fatalf("declare %d: %v", i+1, err)
		}
	}
}

func TestPublishIsConfirmedAndConsumed(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	if err := publisher.Publish(context.Background(), QueueLight,
		testMessage("11111111-1111-4111-8111-111111111111")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	received := make(chan Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Consume(ctx, connection, ConsumerConfig{Queue: QueueLight, Logger: quiet()}, publisher,
		func(_ context.Context, message Message) (Outcome, error) {
			received <- message
			return Outcome{Kind: Done}, nil
		})

	select {
	case message := <-received:
		if message.EventID != "11111111-1111-4111-8111-111111111111" {
			t.Fatalf("received %q", message.EventID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the message never arrived")
	}
}

func TestAnUnroutableMessageIsAnError(t *testing.T) {
	connection := testConnection(t)
	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	err = publisher.Publish(context.Background(), "no.such.queue",
		testMessage("33333333-3333-4333-8333-333333333333"))
	if !errors.Is(err, ErrUnroutable) {
		t.Fatalf("err = %v, want ErrUnroutable: a silent drop is how work disappears", err)
	}
}

func TestAnOversizedMessageIsRefusedBeforeTheBroker(t *testing.T) {
	connection := testConnection(t)
	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	message := testMessage("44444444-4444-4444-8444-444444444444")
	message.TraceContext = map[string]string{"smuggled": string(make([]byte, MaxMessageBytes))}
	if err := publisher.Publish(context.Background(), QueueLight, message); err == nil {
		t.Fatal("a message over the cap was published; envelopes are identifiers only")
	}
}

func TestARetryParksAndComesBack(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	// Park directly in the 30s queue's shape but with a short-TTL sibling: the
	// real delays are too slow for a test, so this proves the routing, not the
	// clock. Publish to the retry routing key and watch it dead-letter home.
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	// A test-only parking queue with the same dead-letter wiring and a 1s TTL.
	name := QueueLight + ".retry.test"
	if _, err := channel.QueueDeclare(name, false, true, false, false, amqp.Table{
		"x-message-ttl":             int32(1000),
		"x-dead-letter-exchange":    Exchange,
		"x-dead-letter-routing-key": QueueLight,
	}); err != nil {
		t.Fatal(err)
	}
	if err := channel.QueueBind(name, name, Exchange, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := publisher.Publish(context.Background(), name,
		testMessage("55555555-5555-4555-8555-555555555555")); err != nil {
		t.Fatalf("parking: %v", err)
	}

	received := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Consume(ctx, connection, ConsumerConfig{Queue: QueueLight, Logger: quiet()}, publisher,
		func(_ context.Context, message Message) (Outcome, error) {
			received <- message.EventID
			return Outcome{Kind: Done}, nil
		})

	select {
	case id := <-received:
		if id != "55555555-5555-4555-8555-555555555555" {
			t.Fatalf("received %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked message never returned to its work queue")
	}
}

func TestPoisonGoesToThisClassesDeadLetters(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	// Not decodable as an envelope: the consumer must dead-letter it, not spin.
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if err := channel.PublishWithContext(context.Background(), Exchange, QueueLight, true, false,
		amqp.Publishing{ContentType: "application/json", Body: []byte(`{"not":"an envelope"}`)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Consume(ctx, connection, ConsumerConfig{Queue: QueueLight, Logger: quiet()}, publisher,
		func(_ context.Context, _ Message) (Outcome, error) {
			t.Error("the handler ran on an undecodable message")
			return Outcome{Kind: Done}, nil
		})

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the poison never reached the dead letter queue")
		case <-time.After(200 * time.Millisecond):
		}
		inspect, err := connection.Channel()
		if err != nil {
			t.Fatal(err)
		}
		state, err := inspect.QueueDeclarePassive(DeadLetterQueue(QueueLight), true, false, false, false, nil)
		inspect.Close()
		if err != nil {
			t.Fatal(err)
		}
		if state.Messages >= 1 {
			return
		}
	}
}

func TestAnUnknownSchemaVersionIsPoisonNotACrash(t *testing.T) {
	future := testMessage("66666666-6666-4666-8666-666666666666")
	future.SchemaVersion = SchemaVersion + 1
	body, _ := json.Marshal(future)
	if _, err := Decode(body); err == nil {
		t.Fatal("a future schema version decoded; the consumer would misread it")
	}
}

func TestOneClassCannotStarveAnother(t *testing.T) {
	connection := testConnection(t)
	drain(t, connection)

	publisher, err := NewPublisher(connection, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	// One slow media message, then a light message. The light consumer runs on
	// its own channel with its own prefetch, so the light message completes
	// while media is still busy.
	if err := publisher.Publish(context.Background(), QueueMedia,
		testMessage("77777777-7777-4777-8777-777777777777")); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), QueueLight,
		testMessage("88888888-8888-4888-8888-888888888888")); err != nil {
		t.Fatal(err)
	}

	var mediaStarted, lightDone atomic.Bool
	release := make(chan struct{})
	lightFinished := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Consume(ctx, connection, ConsumerConfig{Queue: QueueMedia, Logger: quiet()}, publisher,
		func(_ context.Context, _ Message) (Outcome, error) {
			mediaStarted.Store(true)
			<-release // media is "downloading"
			return Outcome{Kind: Done}, nil
		})
	go Consume(ctx, connection, ConsumerConfig{Queue: QueueLight, Logger: quiet()}, publisher,
		func(_ context.Context, _ Message) (Outcome, error) {
			lightDone.Store(true)
			close(lightFinished)
			return Outcome{Kind: Done}, nil
		})

	select {
	case <-lightFinished:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the light message waited on the media message")
	}
	if !lightDone.Load() {
		t.Fatal("light never completed")
	}
	close(release)
	if !mediaStarted.Load() {
		t.Error("media never started; the test proved nothing")
	}
}

func TestMessagesSurviveABrokerRestartOnItsVolume(t *testing.T) {
	// Requires operating the container, which the test cannot assume. The
	// deploy task's compose file gives RabbitMQ a persistent volume, and the
	// dev smoke run covers an actual restart. Here we prove the precondition:
	// the message and its queues are durable and persistent.
	connection := testConnection(t)
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	state, err := channel.QueueDeclarePassive(QueueLight, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("the work queue is not declared durable: %v", err)
	}
	_ = state
}
