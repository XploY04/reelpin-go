package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
)

// FCM is the provider adapter. One client is built on first use and shared:
// it holds credentials and connections, and building one per send would throw
// both away.
type FCM struct {
	once            sync.Once
	client          *messaging.Client
	err             error
	credentialsJSON string
	projectID       string
	timeout         time.Duration
}

func NewFCM(credentialsJSON, projectID string, timeout time.Duration) *FCM {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &FCM{credentialsJSON: credentialsJSON, projectID: projectID, timeout: timeout}
}

func (f *FCM) connect(ctx context.Context) (*messaging.Client, error) {
	f.once.Do(func() {
		if strings.TrimSpace(f.credentialsJSON) == "" {
			f.err = fmt.Errorf("firebase credentials are not configured")
			return
		}
		app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: f.projectID},
			option.WithCredentialsJSON([]byte(f.credentialsJSON)))
		if err != nil {
			f.err = fmt.Errorf("building the firebase app: %w", err)
			return
		}
		f.client, f.err = app.Messaging(ctx)
	})
	return f.client, f.err
}

// Send delivers to every token in bounded batches and reports what happened to
// each one, so dead tokens can be removed and transient failures retried.
func (f *FCM) Send(ctx context.Context, tokens []string, message Message) ([]Delivery, error) {
	client, err := f.connect(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	deliveries := make([]Delivery, 0, len(tokens))
	for start := 0; start < len(tokens); start += MaxTokensPerBatch {
		end := start + MaxTokensPerBatch
		if end > len(tokens) {
			end = len(tokens)
		}
		batch := tokens[start:end]

		response, err := client.SendEachForMulticast(ctx, &messaging.MulticastMessage{
			Tokens: batch,
			Notification: &messaging.Notification{
				Title: message.Title,
				Body:  message.Body,
			},
			Data:    message.Data,
			Android: &messaging.AndroidConfig{Priority: "high"},
			APNS: &messaging.APNSConfig{
				Payload: &messaging.APNSPayload{Aps: &messaging.Aps{Sound: "default"}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("sending to the push provider: %w", err)
		}

		for index, result := range response.Responses {
			delivery := Delivery{Token: batch[index]}
			switch {
			case result.Success:
				delivery.MessageID = result.MessageID
			case messaging.IsUnregistered(result.Error), messaging.IsInvalidArgument(result.Error):
				// The app was uninstalled or the token was rotated.
				delivery.Invalid = true
				delivery.Err = result.Error
			default:
				delivery.Retryable = true
				delivery.Err = result.Error
			}
			deliveries = append(deliveries, delivery)
		}
	}
	return deliveries, nil
}
