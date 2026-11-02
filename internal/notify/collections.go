package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func EnqueueCollectionUpdate(ctx context.Context, tx pgx.Tx, collectionID, actorID string, added int) error {
	if added == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT user_id::text FROM (
		SELECT owner_id AS user_id FROM reelpin.collections WHERE id=$1
		UNION SELECT user_id FROM reelpin.collection_members WHERE collection_id=$1
		) recipients WHERE user_id<>$2::uuid`, collectionID, actorID)
	if err != nil {
		return fmt.Errorf("reading collection notification recipients: %w", err)
	}
	var recipients []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return err
		}
		recipients = append(recipients, userID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, userID := range recipients {
		notification := CollectionUpdated(userID, collectionID, "", added)
		notification.Data["change"] = "items_added"
		if err := enqueueCollection(ctx, tx, notification); err != nil {
			return err
		}
	}
	return nil
}

func EnqueueCollectionJoin(ctx context.Context, tx pgx.Tx, collectionID, ownerID string) error {
	notification := CollectionUpdated(ownerID, collectionID, "", 0)
	notification.Title = "New collaborator"
	notification.Body = "A collaborator joined your collection."
	notification.Data["change"] = "member_joined"
	return enqueueCollection(ctx, tx, notification)
}

func enqueueCollection(ctx context.Context, tx pgx.Tx, notification Notification) error {
	id := uuid.NewString()
	data, err := json.Marshal(notification.Data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO reelpin.notifications(id,user_id,event_key,kind,title,body,data)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, id, notification.UserID,
		EventKey(notification.Type, notification.UserID, id), notification.Type, notification.Title, notification.Body, data)
	if err != nil {
		return fmt.Errorf("recording collection notification: %w", err)
	}
	return outbox.Insert(ctx, tx, outbox.Event{
		EventID: id, EventType: queue.EventNotification, RoutingKey: queue.QueueNotify,
	})
}

func (s *Service) SendCollectionUpdate(ctx context.Context, notificationID string) (string, error) {
	var notification Notification
	var data []byte
	var accessible bool
	err := s.pool.QueryRow(ctx, `SELECT n.user_id::text,n.event_key,n.kind,n.title,n.body,n.data,
		EXISTS(SELECT 1 FROM reelpin.collections c WHERE c.id::text=n.data->>'collection_id'
		AND (c.owner_id=n.user_id OR EXISTS(SELECT 1 FROM reelpin.collection_members m
		WHERE m.collection_id=c.id AND m.user_id=n.user_id)))
		FROM reelpin.notifications n WHERE n.id=$1 AND n.kind='collection_update'`, notificationID).
		Scan(&notification.UserID, &notification.EventKey, &notification.Type, &notification.Title, &notification.Body, &data, &accessible)
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationID, ErrNotFound
	}
	if err != nil {
		return notificationID, fmt.Errorf("reading collection notification: %w", err)
	}
	if !accessible {
		_, err := s.pool.Exec(ctx, `UPDATE reelpin.notifications SET status='failed',
			failure_reason='collection is no longer accessible',updated_at=now()
			WHERE id=$1 AND status<>'sent'`, notificationID)
		if err != nil {
			return notificationID, err
		}
		return notificationID, ErrNotFound
	}
	if err := json.Unmarshal(data, &notification.Data); err != nil {
		return notificationID, fmt.Errorf("decoding collection notification: %w", err)
	}
	return s.SendToUser(ctx, notification)
}
