package netx

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/situker/qqmail-cli/internal/errmap"
)

const (
	// DefaultMaxAttempts permits at most 2 extra retries (3 attempts total).
	DefaultMaxAttempts = 3
	DefaultBaseBackoff = 500 * time.Millisecond
	DefaultMaxBackoff  = 4 * time.Second
)

// RetryConfig configures bounded exponential backoff retries.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts (1 = no retries, <=0 uses DefaultMaxAttempts).
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	NoJitter    bool
	Sleep       func(context.Context, time.Duration) error
}

func (c RetryConfig) maxAttempts() int {
	if c.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return c.MaxAttempts
}

func (c RetryConfig) baseBackoff() time.Duration {
	if c.BaseBackoff <= 0 {
		return DefaultBaseBackoff
	}
	return c.BaseBackoff
}

func (c RetryConfig) maxBackoff() time.Duration {
	if c.MaxBackoff <= 0 {
		return DefaultMaxBackoff
	}
	return c.MaxBackoff
}

func (c RetryConfig) delayForRetry(retryIndex int) time.Duration {
	delay := c.baseBackoff()
	for i := 1; i < retryIndex; i++ {
		if delay >= c.maxBackoff()/2 {
			delay = c.maxBackoff()
			break
		}
		delay *= 2
	}
	if delay > c.maxBackoff() {
		delay = c.maxBackoff()
	}
	if !c.NoJitter && delay > 0 {
		// Bounded downward jitter in [0.75*delay, delay] to avoid herd spikes
		// without exceeding MaxBackoff.
		quarter := int64(delay / 4)
		if quarter > 0 {
			delay = delay - time.Duration(rand.Int64N(quarter+1))
		}
	}
	return delay
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retry executes fn up to cfg.MaxAttempts times while errors are explicitly
// retryable and the context deadline has not expired. It annotates any returned
// error with the total attempt count.
func Retry[T any](ctx context.Context, cfg RetryConfig, fn func(ctx context.Context, attempt int) (T, error)) (T, int, error) {
	var zero T
	maxAttempts := cfg.maxAttempts()
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepWithContext
	}

	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		value, err := fn(ctx, attempt)
		if err == nil {
			return value, attempts, nil
		}
		lastErr = err
		if attempt == maxAttempts || !IsRetryable(err) || ctx.Err() != nil {
			break
		}
		delay := cfg.delayForRetry(attempt)
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) <= delay {
				// Respect overall deadline: do not sleep past or right up to it.
				break
			}
		}
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			break
		}
	}
	return zero, attempts, errmap.Classify(lastErr).WithContext(map[string]any{"attempts": attempts})
}
