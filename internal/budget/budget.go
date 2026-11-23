// Package budget enforces monthly spending caps per project and per API key.
//
// Caps are checked with a reserve-then-settle scheme: before a call, the
// worst-case cost (all input at full price plus max_tokens of output) is
// reserved atomically against every applicable cap in one Redis script.
// After the call, the reservation is replaced with the real cost. Concurrent
// requests therefore can't jointly overspend a cap, and a cap is never
// exceeded by more than rounding.
package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Scope struct {
	Kind string  // "project" or "key"
	ID   string  //
	Cap  float64 // USD per month; 0 = no cap
}

// SpendLoader returns what a scope has spent this month according to the
// request log. It seeds Redis when the counter is missing (first request of
// the month, or Redis was flushed).
type SpendLoader func(ctx context.Context, kind, id string, monthStart time.Time) (float64, error)

type Budget struct {
	RDB  *redis.Client
	Load SpendLoader
	Now  func() time.Time
}

type ExceededError struct {
	Scope   Scope
	Spent   float64
	Request float64
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("%s %s has spent $%.4f of its $%.2f monthly cap; this request could cost up to $%.4f",
		e.Scope.Kind, e.Scope.ID, e.Spent, e.Scope.Cap, e.Request)
}

type Reservation struct {
	keys   []string
	amount float64
}

// reserve checks every cap first and only then increments, all inside one
// script, so two requests can't both pass a check that only one fits under.
var reserve = redis.NewScript(`
local amt = tonumber(ARGV[1])
for i = 1, #KEYS do
  local cap = tonumber(ARGV[i + 1])
  if cap > 0 then
    local cur = tonumber(redis.call('GET', KEYS[i]) or '0')
    if cur + amt > cap then
      return {i, tostring(cur)}
    end
  end
end
for i = 1, #KEYS do
  redis.call('INCRBYFLOAT', KEYS[i], amt)
  redis.call('EXPIRE', KEYS[i], ARGV[#KEYS + 2])
end
return {0, '0'}
`)

func (b *Budget) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (b *Budget) key(s Scope) string {
	return fmt.Sprintf("spend:%s:%s:%s", s.Kind, s.ID, b.now().UTC().Format("2006-01"))
}

// Spent returns the counter for a scope this month (reservations included).
func (b *Budget) Spent(ctx context.Context, s Scope) (float64, error) {
	if err := b.seed(ctx, s); err != nil {
		return 0, err
	}
	return b.RDB.Get(ctx, b.key(s)).Float64()
}

func (b *Budget) seed(ctx context.Context, s Scope) error {
	k := b.key(s)
	n, err := b.RDB.Exists(ctx, k).Result()
	if err != nil || n == 1 {
		return err
	}
	spent, err := b.Load(ctx, s.Kind, s.ID, monthStart(b.now()))
	if err != nil {
		return err
	}
	// SETNX: if another request seeded it meanwhile, keep that value.
	return b.RDB.SetNX(ctx, k, spent, 40*24*time.Hour).Err()
}

// Reserve holds amount USD against every scope, or fails without holding
// anything if one cap would be exceeded.
func (b *Budget) Reserve(ctx context.Context, scopes []Scope, amount float64) (*Reservation, error) {
	keys := make([]string, len(scopes))
	args := []any{amount}
	for i, s := range scopes {
		if err := b.seed(ctx, s); err != nil {
			return nil, err
		}
		keys[i] = b.key(s)
		args = append(args, s.Cap)
	}
	args = append(args, int((40 * 24 * time.Hour).Seconds()))
	res, err := reserve.Run(ctx, b.RDB, keys, args...).Slice()
	if err != nil {
		return nil, err
	}
	if idx := res[0].(int64); idx > 0 {
		var spent float64
		fmt.Sscan(res[1].(string), &spent)
		return nil, &ExceededError{Scope: scopes[idx-1], Spent: spent, Request: amount}
	}
	return &Reservation{keys: keys, amount: amount}, nil
}

// Settle replaces the reservation with the actual cost.
func (b *Budget) Settle(ctx context.Context, r *Reservation, actual float64) error {
	if r == nil {
		return nil
	}
	delta := actual - r.amount
	if delta == 0 {
		return nil
	}
	pipe := b.RDB.Pipeline()
	for _, k := range r.keys {
		pipe.IncrByFloat(ctx, k, delta)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Limiter is a fixed-window requests-per-minute limit per API key.
type Limiter struct {
	RDB *redis.Client
}

// Allow increments the key's counter for the current minute and reports
// whether it is within rpm, plus the seconds until the window resets.
func (l *Limiter) Allow(ctx context.Context, keyID string, rpm int) (bool, int, error) {
	now := time.Now()
	k := fmt.Sprintf("rl:%s:%d", keyID, now.Unix()/60)
	pipe := l.RDB.Pipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 70*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, err
	}
	return incr.Val() <= int64(rpm), 60 - int(now.Unix()%60), nil
}
