package gateway

import (
	"sync"
	"time"

	"web3-amm-dex-backend/ledger"
)

// RateLimiter is a per-account token bucket. Accounts are independent: one
// account exhausting its bucket never starves another. rate <= 0 disables
// limiting entirely.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens map[ledger.AccountID]float64
	last   map[ledger.AccountID]time.Time
}

func NewRateLimiter(rate, burst float64) *RateLimiter {
	return &RateLimiter{
		rate:   rate,
		burst:  burst,
		tokens: make(map[ledger.AccountID]float64),
		last:   make(map[ledger.AccountID]time.Time),
	}
}

// Allow consumes one token for the account. It returns false when the bucket is
// empty (the caller should respond 429). Buckets are created lazily, full.
func (r *RateLimiter) Allow(a ledger.AccountID) bool {
	if r.rate <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	tokens := r.tokens[a]
	if last, ok := r.last[a]; ok {
		tokens += now.Sub(last).Seconds() * r.rate
		if tokens > r.burst {
			tokens = r.burst
		}
	} else {
		tokens = r.burst
	}
	r.last[a] = now
	if tokens < 1 {
		r.tokens[a] = tokens
		return false
	}
	r.tokens[a] = tokens - 1
	return true
}
