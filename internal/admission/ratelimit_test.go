package admission

import (
	"fmt"
	"testing"
	"time"
)

func TestProjectRateLimiter(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	l := NewProjectRateLimiter(0.5, 2)
	l.now = func() time.Time { return now }

	for i := range 2 {
		if ok, _ := l.Allow("project-a"); !ok {
			t.Fatalf("create %d within the burst was refused", i+1)
		}
	}
	ok, wait := l.Allow("project-a")
	if ok {
		t.Fatal("create beyond the burst was allowed")
	}
	if wait <= 0 || wait > 2*time.Second {
		t.Errorf("wait = %v, want (0, 2s]", wait)
	}

	// Another project has its own bucket.
	if ok, _ := l.Allow("project-b"); !ok {
		t.Error("project-b was limited by project-a's creates")
	}

	// A refused create does not consume a token: after one token's worth of
	// time exactly one more create fits.
	now = now.Add(2 * time.Second)
	if ok, _ := l.Allow("project-a"); !ok {
		t.Error("create after refill was refused")
	}
	if ok, _ := l.Allow("project-a"); ok {
		t.Error("second create after a single token's refill was allowed")
	}
}

func TestProjectRateLimiterPrunesIdleProjects(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	l := NewProjectRateLimiter(1, 1)
	l.now = func() time.Time { return now }
	for i := range maxIdleLimiters {
		l.Allow(fmt.Sprintf("project-%d", i))
	}
	// project-0 is still limited until its bucket refills.
	if ok, _ := l.Allow("project-0"); ok {
		t.Fatal("project-0 was not limited")
	}
	now = now.Add(time.Minute)
	l.Allow("project-new")
	if n := len(l.projects); n != 1 {
		t.Errorf("%d limiters kept after every idle bucket refilled, want 1", n)
	}
}
