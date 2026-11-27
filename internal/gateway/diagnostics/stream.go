package diagnostics

import (
	"errors"
	"fmt"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

// errViolation marks a backend stream that broke the contract's grammar.
var errViolation = errors.New("backend stream violates the contract")

func violation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errViolation, fmt.Sprintf(format, args...))
}

// tracker checks a RunDiagnostic stream against the contract's grammar (one
// snapshot, then one outcome per selected router, then one complete) and
// counts what arrived.
type tracker struct {
	requestID string
	query     backendv1.DiagnosticType
	seq       uint64

	snapshot *backendv1.Snapshot
	// pending holds the selected routers with no outcome yet.
	pending  map[string]bool
	selected int

	successful, failed, truncated int
	complete                      *backendv1.Complete
}

func newTracker(req *backendv1.RunDiagnosticRequest) *tracker {
	return &tracker{requestID: req.GetRequestId(), query: req.GetQuery().GetType()}
}

// accept checks ev against the stream so far and records it. Any error it
// returns wraps errViolation.
func (t *tracker) accept(ev *backendv1.DiagnosticEvent) error {
	if ev.GetRequestId() != t.requestID {
		return violation("event for another request")
	}
	if ev.GetSequence() != t.seq+1 {
		return violation("sequence %d after %d", ev.GetSequence(), t.seq)
	}
	t.seq++
	if t.complete != nil {
		return violation("event after complete")
	}
	switch e := ev.GetEvent().(type) {
	case *backendv1.DiagnosticEvent_Snapshot:
		return t.acceptSnapshot(e.Snapshot)
	case *backendv1.DiagnosticEvent_Outcome:
		return t.acceptOutcome(e.Outcome)
	case *backendv1.DiagnosticEvent_Complete:
		return t.acceptComplete(e.Complete)
	default:
		return violation("event has no payload")
	}
}

func (t *tracker) acceptSnapshot(s *backendv1.Snapshot) error {
	if t.snapshot != nil {
		return violation("second snapshot")
	}
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, r := range s.GetRouters() {
		if r.GetRouter() == "" || seen[r.GetRouter()] {
			return violation("snapshot router names must be unique and non-empty")
		}
		seen[r.GetRouter()] = true
		if r.GetSelected() {
			pending[r.GetRouter()] = true
		}
	}
	t.snapshot, t.pending, t.selected = s, pending, len(pending)
	return nil
}

func (t *tracker) acceptOutcome(o *backendv1.RouterOutcome) error {
	if t.snapshot == nil {
		return violation("outcome before the snapshot")
	}
	if !t.pending[o.GetRouter()] {
		return violation("outcome for a router that is not selected or already answered")
	}
	if o.GetCompletedAt() == nil {
		return violation("outcome has no completion time")
	}
	switch {
	case o.GetObservation() != nil:
		if o.GetObservation().GetTruncated() {
			t.truncated++
		}
		t.successful++
	case o.GetFailure() != nil:
		t.failed++
	default:
		return violation("outcome has neither an observation nor a failure")
	}
	delete(t.pending, o.GetRouter())
	return nil
}

func (t *tracker) acceptComplete(c *backendv1.Complete) error {
	if t.snapshot == nil {
		return violation("complete before the snapshot")
	}
	if len(t.pending) > 0 {
		return violation("complete with %d selected routers unanswered", len(t.pending))
	}
	routers := len(t.snapshot.GetRouters())
	switch {
	case c.GetSnapshotId() != t.snapshot.GetSnapshotId():
		return violation("complete names another snapshot")
	case int(c.GetExpected()) != routers,
		int(c.GetSuccessful()) != t.successful,
		int(c.GetFailed()) != t.failed,
		int(c.GetOmitted()) != routers-t.selected,
		int(c.GetTruncated()) != t.truncated:
		return violation("complete counts disagree with the stream")
	}
	t.complete = c
	return nil
}

// coverage reports what arrived. Complete is true only after a complete
// event the tracker accepted.
func (t *tracker) coverage() *v1alpha.Coverage {
	if t.snapshot == nil {
		return nil
	}
	routers := len(t.snapshot.GetRouters())
	return &v1alpha.Coverage{
		Complete:   t.complete != nil,
		Expected:   int32(routers),
		Selected:   int32(t.selected),
		Successful: int32(t.successful),
		Failed:     int32(t.failed),
		Omitted:    int32(routers - t.selected),
		Truncated:  int32(t.truncated),
	}
}
