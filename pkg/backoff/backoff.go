// Package backoff computes retry delays with full jitter.
package backoff

import (
	"math"
	"math/rand"
	"time"
)

// Policy describes an exponential backoff schedule.
type Policy struct {
	Base   time.Duration
	Max    time.Duration
	Factor float64
	// Jitter spreads retries so a provider outage does not produce a
	// synchronised thundering herd when it recovers.
	Jitter bool
}

// Default is the policy used by the outbound scheduler.
func Default(base, max time.Duration) Policy {
	if base <= 0 {
		base = 30 * time.Second
	}
	if max <= 0 || max < base {
		max = 30 * time.Minute
	}
	return Policy{Base: base, Max: max, Factor: 2, Jitter: true}
}

// Delay returns the wait before attempt number n (1-based: the delay after the
// first failure is Delay(1)).
func (p Policy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	factor := p.Factor
	if factor <= 1 {
		factor = 2
	}

	backoffFloat := float64(p.Base) * math.Pow(factor, float64(attempt-1))
	if math.IsInf(backoffFloat, 0) || backoffFloat > float64(p.Max) {
		backoffFloat = float64(p.Max)
	}

	d := time.Duration(backoffFloat)
	if d > p.Max {
		d = p.Max
	}
	if !p.Jitter {
		return d
	}

	// Keep at least half the nominal delay so a retry never comes back
	// instantly and hammers a struggling provider.
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// NextAttemptAt returns the absolute time of the next retry.
func (p Policy) NextAttemptAt(now time.Time, attempt int) time.Time {
	return now.Add(p.Delay(attempt))
}
