//go:build integration

package reminders

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testSchedule(t *testing.T) *RedisSchedule {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parsing TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis is not reachable: %v", err)
	}
	schedule := NewRedisSchedule(client, fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano()))
	t.Cleanup(func() { client.Del(context.Background(), schedule.key) })
	return schedule
}

func TestTheRedisScheduleAnswersOnlyWhatIsDue(t *testing.T) {
	schedule := testSchedule(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	for id, at := range map[string]time.Time{
		"early": now.Add(-time.Hour), "just": now, "later": now.Add(time.Millisecond),
	} {
		if err := schedule.Add(ctx, id, at); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	due, err := schedule.Due(ctx, now, 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 2 || due[0] != "early" || due[1] != "just" {
		t.Fatalf("due = %v, want early then just", due)
	}
	if limited, _ := schedule.Due(ctx, now, 1); len(limited) != 1 {
		t.Fatalf("limit ignored: %v", limited)
	}

	first, err := schedule.Take(ctx, "early")
	if err != nil || !first {
		t.Fatalf("first take = %v, %v", first, err)
	}
	if second, _ := schedule.Take(ctx, "early"); second {
		t.Fatal("a member was taken twice")
	}

	if err := schedule.Remove(ctx, "just"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if due, _ := schedule.Due(ctx, now.Add(time.Hour), 10); len(due) != 1 || due[0] != "later" {
		t.Fatalf("due = %v, want only later", due)
	}
}
