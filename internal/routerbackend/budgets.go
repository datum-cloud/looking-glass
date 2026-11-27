package routerbackend

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/time/rate"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
)

// maxIdleProjects is how many per-project budgets are kept before idle ones
// (no live diagnostic and a full bucket) are dropped. A dropped budget is
// recreated full, which is what it would have been anyway.
const maxIdleProjects = 1024

// liveRetryAfter is the retry advice for a project at its live diagnostic
// bound. No one knows when a live diagnostic ends, so it is a fixed pause.
const liveRetryAfter = 5 * time.Second

// Budget scopes, as Failure.scope names them.
const (
	scopeCell    = "cell"
	scopeProject = "project"
	scopeRouter  = "router"
)

// requestBudgets applies the cell and project budgets to each request before
// its snapshot.
type requestBudgets struct {
	cell         *rate.Limiter
	projectRate  rate.Limit
	projectBurst int
	liveMax      int
	now          func() time.Time

	mu       sync.Mutex
	projects map[string]*projectBudget
}

type projectBudget struct {
	limiter *rate.Limiter
	live    int
}

// throttled says why a request was refused and when to try again.
type throttled struct {
	scope      string
	retryAfter time.Duration
}

// admit takes one token from the cell and project buckets and one live slot
// from the project, or none of them. On success it returns the function that
// gives the live slot back.
func (b *requestBudgets) admit(project string) (release func(), refused *throttled) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	p, ok := b.projects[project]
	if !ok {
		if len(b.projects) >= maxIdleProjects {
			b.pruneLocked(now)
		}
		p = &projectBudget{limiter: rate.NewLimiter(b.projectRate, b.projectBurst)}
		b.projects[project] = p
	}
	if p.live >= b.liveMax {
		return nil, &throttled{scope: scopeProject, retryAfter: liveRetryAfter}
	}
	cell := b.cell.ReserveN(now, 1)
	if d := cell.DelayFrom(now); !cell.OK() || d > 0 {
		cell.CancelAt(now)
		return nil, &throttled{scope: scopeCell, retryAfter: d}
	}
	proj := p.limiter.ReserveN(now, 1)
	if d := proj.DelayFrom(now); !proj.OK() || d > 0 {
		proj.CancelAt(now)
		cell.CancelAt(now)
		return nil, &throttled{scope: scopeProject, retryAfter: d}
	}
	p.live++
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		p.live--
	}, nil
}

func (b *requestBudgets) pruneLocked(now time.Time) {
	for name, p := range b.projects {
		if p.live == 0 && p.limiter.TokensAt(now) >= float64(b.projectBurst) {
			delete(b.projects, name)
		}
	}
}

// routerSlots bounds the diagnostics one router runs at once and the ones
// waiting for it.
type routerSlots struct {
	running chan struct{}
	depth   int
	wait    time.Duration

	mu      sync.Mutex
	waiting int
}

func newRouterSlots(concurrency, depth int, wait time.Duration) *routerSlots {
	return &routerSlots{running: make(chan struct{}, concurrency), depth: depth, wait: wait}
}

// acquire takes a slot. It fails with QueueFull when the queue is full,
// QueueTimeout when no slot frees within the queue wait, and the context's
// reason when ctx ends first.
func (s *routerSlots) acquire(ctx context.Context) (release func(), reason string) {
	release = func() { <-s.running }
	select {
	case s.running <- struct{}{}:
		return release, ""
	default:
	}
	s.mu.Lock()
	if s.waiting >= s.depth {
		s.mu.Unlock()
		return nil, backendv1.ReasonQueueFull
	}
	s.waiting++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.waiting--
		s.mu.Unlock()
	}()

	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case s.running <- struct{}{}:
		return release, ""
	case <-timer.C:
		return nil, backendv1.ReasonQueueTimeout
	case <-ctx.Done():
		return nil, stopReason(ctx)
	}
}

// stopReason is the reason a diagnostic's context ended: DeadlineExceeded
// when expires_at passed, Cancelled otherwise.
func stopReason(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errRequestExpired) {
		return backendv1.ReasonDeadlineExceeded
	}
	return backendv1.ReasonCancelled
}
