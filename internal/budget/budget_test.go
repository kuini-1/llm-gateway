package budget

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("GATEWAY_TEST_REDIS")
	if url == "" {
		t.Skip("set GATEWAY_TEST_REDIS (e.g. redis://127.0.0.1:6389/15)")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func TestCapsHoldUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	b := &Budget{RDB: testRedis(t), Load: func(context.Context, string, string, time.Time) (float64, error) { return 0, nil }}
	scopes := []Scope{{Kind: "project", ID: "p1", Cap: 1.00}, {Kind: "key", ID: "k1", Cap: 0}}

	// 200 concurrent reservations of $0.01 against a $1.00 cap: exactly 100 fit.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Reserve(ctx, scopes, 0.01)
			mu.Lock()
			defer mu.Unlock()
			var ex *ExceededError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ex):
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 100 || refused != 100 {
		t.Fatalf("ok %d, refused %d; want 100/100", ok, refused)
	}
	spent, _ := b.Spent(ctx, scopes[0])
	if math.Abs(spent-1.00) > 1e-9 {
		t.Errorf("project spent %.6f", spent)
	}
	// The uncapped key scope counted everything that was allowed.
	if s, _ := b.Spent(ctx, scopes[1]); math.Abs(s-1.00) > 1e-9 {
		t.Errorf("key spent %.6f", s)
	}
}

func TestSettleReplacesReservation(t *testing.T) {
	ctx := context.Background()
	b := &Budget{RDB: testRedis(t), Load: func(context.Context, string, string, time.Time) (float64, error) { return 0.25, nil }}
	s := []Scope{{Kind: "project", ID: "p2", Cap: 1}}
	r, err := b.Reserve(ctx, s, 0.50) // worst case
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(ctx, r, 0.02); err != nil { // what it really cost
		t.Fatal(err)
	}
	// Seeded from the log ($0.25) + actual ($0.02).
	if spent, _ := b.Spent(ctx, s[0]); math.Abs(spent-0.27) > 1e-9 {
		t.Errorf("spent %.6f, want 0.27", spent)
	}
	// A reservation that doesn't fit is refused and holds nothing.
	if _, err := b.Reserve(ctx, s, 0.80); err == nil {
		t.Fatal("expected cap exceeded")
	}
	if spent, _ := b.Spent(ctx, s[0]); math.Abs(spent-0.27) > 1e-9 {
		t.Errorf("a refused reservation changed the counter: %.6f", spent)
	}
}

func TestMonthRollsOver(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	b := &Budget{RDB: testRedis(t), Now: func() time.Time { return now },
		Load: func(context.Context, string, string, time.Time) (float64, error) { return 0, nil }}
	s := []Scope{{Kind: "key", ID: "k9", Cap: 0.10}}
	if _, err := b.Reserve(ctx, s, 0.10); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, s, 0.01); err == nil {
		t.Fatal("cap should be full in November")
	}
	now = now.Add(2 * time.Minute) // December
	if _, err := b.Reserve(ctx, s, 0.05); err != nil {
		t.Fatalf("new month should start empty: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	ctx := context.Background()
	l := &Limiter{RDB: testRedis(t)}
	allowed := 0
	for i := 0; i < 5; i++ {
		if ok, _, err := l.Allow(ctx, "k", 3); err != nil {
			t.Fatal(err)
		} else if ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed %d of 5 with rpm 3", allowed)
	}
}
