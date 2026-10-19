package reminders

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisSchedule is the due set as one sorted set: the member is a reminder
// id, the score its fire time in Unix milliseconds. Losing it loses nothing
// durable; a worker re-seeds it from PostgreSQL when it starts.
type RedisSchedule struct {
	client redis.Cmdable
	key    string
}

func NewRedisSchedule(client redis.Cmdable, prefix string) *RedisSchedule {
	if prefix == "" {
		prefix = "reelpin"
	}
	return &RedisSchedule{client: client, key: prefix + ":reminders:due"}
}

func (s *RedisSchedule) Add(ctx context.Context, reminderID string, fireAt time.Time) error {
	if err := s.client.ZAdd(ctx, s.key, redis.Z{Score: float64(fireAt.UnixMilli()), Member: reminderID}).Err(); err != nil {
		return fmt.Errorf("scheduling a reminder: %w", err)
	}
	return nil
}

func (s *RedisSchedule) Remove(ctx context.Context, reminderID string) error {
	if err := s.client.ZRem(ctx, s.key, reminderID).Err(); err != nil {
		return fmt.Errorf("unscheduling a reminder: %w", err)
	}
	return nil
}

func (s *RedisSchedule) Due(ctx context.Context, now time.Time, limit int) ([]string, error) {
	ids, err := s.client.ZRangeByScore(ctx, s.key, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(now.UnixMilli(), 10),
		Count: int64(limit),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("reading due reminders: %w", err)
	}
	return ids, nil
}

func (s *RedisSchedule) Take(ctx context.Context, reminderID string) (bool, error) {
	removed, err := s.client.ZRem(ctx, s.key, reminderID).Result()
	if err != nil {
		return false, fmt.Errorf("taking a due reminder: %w", err)
	}
	return removed == 1, nil
}
