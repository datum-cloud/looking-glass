package admission

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxIdleLimiters is how many per-project limiters are kept before idle ones
// (with a full bucket) are dropped. A dropped limiter is recreated full, which
// is what it would have been anyway.
const maxIdleLimiters = 1024

// ProjectRateLimiter limits session creates per project with a token bucket.
// It is per process: with several webhook replicas, a project's effective
// limit is the per-replica limit times the replicas it reaches.
type ProjectRateLimiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu       sync.Mutex
	projects map[string]*rate.Limiter
}

// NewProjectRateLimiter returns a limiter allowing qps creates per second per
// project, with bursts of up to burst.
func NewProjectRateLimiter(qps float64, burst int) *ProjectRateLimiter {
	return &ProjectRateLimiter{
		limit:    rate.Limit(qps),
		burst:    burst,
		now:      time.Now,
		projects: map[string]*rate.Limiter{},
	}
}

// Allow reports whether project may create a session now and consumes a token
// if so. When it may not, it returns how long until a token is available.
func (l *ProjectRateLimiter) Allow(project string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	lim, ok := l.projects[project]
	if !ok {
		if len(l.projects) >= maxIdleLimiters {
			l.pruneLocked(now)
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.projects[project] = lim
	}
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, 0
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

func (l *ProjectRateLimiter) pruneLocked(now time.Time) {
	for p, lim := range l.projects {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.projects, p)
		}
	}
}
