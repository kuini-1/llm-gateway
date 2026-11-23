package router

import (
	"sync"
	"time"
)

// State of a circuit breaker.
type State string

const (
	Closed   State = "closed"    // requests flow
	Open     State = "open"      // the target is skipped
	HalfOpen State = "half_open" // one probe request is allowed through
)

// Breaker stops sending traffic to a target after repeated failures, so a
// dead provider costs one timeout per cooldown instead of one per request.
type Breaker struct {
	mu        sync.Mutex
	threshold int
	openFor   time.Duration
	now       func() time.Time

	state       State
	failures    int // consecutive
	openedAt    time.Time
	probing     bool
	lastError   string
	transitions int
}

func NewBreaker(threshold int, openFor time.Duration) *Breaker {
	return &Breaker{threshold: threshold, openFor: openFor, now: time.Now, state: Closed}
}

// Allow reports whether a request may go to the target now.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) < b.openFor {
			return false
		}
		b.state, b.probing = HalfOpen, true
		b.transitions++
		return true
	default: // half-open: only one probe at a time
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Closed {
		b.transitions++
	}
	b.state, b.failures, b.probing = Closed, 0, false
}

func (b *Breaker) Failure(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.lastError = reason
	if b.state == HalfOpen || b.failures >= b.threshold {
		if b.state != Open {
			b.transitions++
		}
		b.state, b.openedAt, b.probing = Open, b.now(), false
	}
}

type BreakerStatus struct {
	State       State     `json:"state"`
	Failures    int       `json:"consecutive_failures"`
	OpenedAt    time.Time `json:"opened_at,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Transitions int       `json:"transitions"`
}

func (b *Breaker) Status() BreakerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BreakerStatus{State: b.state, Failures: b.failures, LastError: b.lastError, Transitions: b.transitions}
	if b.state != Closed {
		s.OpenedAt = b.openedAt
	}
	return s
}
