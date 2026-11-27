// Package session reconciles LookingGlassSessions in a project control plane:
// it binds each session to the cell that serves its location, enforces the
// deadlines the control plane owns, and deletes terminal sessions once their
// retention ends.
package session

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lookingglassv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

// Deadlines from the contract's Deadlines section.
const (
	// PlacementTimeout runs from creation to SessionReady. On expiry the
	// control plane ends the session as Unavailable.
	PlacementTimeout = 30 * time.Second
	// ConnectionWindow runs from SessionReady to claim. The cell enforces
	// it; the control plane uses it to bound the connectBefore a cell
	// reports.
	ConnectionWindow = 60 * time.Second
	// ExecutionTimeout runs from claim to the backend's end. The cell
	// enforces it; the control plane uses it only for the AgentLost backstop.
	ExecutionTimeout = 90 * time.Second
	// AgentLostMargin is added to the latest moment a claimed session can
	// still be running, so status still in transit from the cell is not cut
	// short.
	AgentLostMargin = 60 * time.Second
	// Retention is how long a terminal session is kept after FinishedAt.
	Retention = 15 * time.Minute
)

// Errors MergeCellStatus returns when it ignores a cell's status.
var (
	// ErrNotBound means the session has no cell yet. A cell can never bind a
	// session to itself.
	ErrNotBound = errors.New("session is not bound to a cell")
	// ErrWrongCell means the status came from, or names, a cell other than
	// the bound one.
	ErrWrongCell = errors.New("status is not from the bound cell")
	// ErrInvalidStatus means the reported Ready condition is outside the
	// contract's reason set, or SessionReady lacks its connection details.
	ErrInvalidStatus = errors.New("invalid cell status")
)

// IsTerminal reports whether the session has a terminal reason.
func IsTerminal(s *lookingglassv1alpha.LookingGlassSession) bool {
	ready := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	return ready != nil && ready.Status == metav1.ConditionFalse
}

// PlacementDeadline is when a session that has not reached SessionReady ends
// as Unavailable.
func PlacementDeadline(s *lookingglassv1alpha.LookingGlassSession) time.Time {
	return s.CreationTimestamp.Add(PlacementTimeout)
}

// AgentLostDeadline is when a session that is still not terminal ends as
// AgentLost: connectBefore + 90 s + 60 s. A connectable session without
// connectBefore, which MergeCellStatus never writes, is anchored at its Ready
// transition plus the connection window instead, so the backstop always
// applies. It is false while the session is still Pending.
func AgentLostDeadline(s *lookingglassv1alpha.LookingGlassSession) (time.Time, bool) {
	if s.Status.ConnectBefore != nil {
		return s.Status.ConnectBefore.Add(ExecutionTimeout + AgentLostMargin), true
	}
	ready := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	if rank(ready) == 0 {
		return time.Time{}, false
	}
	return ready.LastTransitionTime.Add(ConnectionWindow + ExecutionTimeout + AgentLostMargin), true
}

// SetPending writes Ready=Unknown/Pending if the session has no Ready
// condition for its current generation. It reports whether it changed the
// status.
func SetPending(s *lookingglassv1alpha.LookingGlassSession, now time.Time) bool {
	ready := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	if ready != nil && ready.ObservedGeneration >= s.Generation {
		return false
	}
	s.Status.ObservedGeneration = s.Generation
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type:               lookingglassv1alpha.ConditionReady,
		Status:             metav1.ConditionUnknown,
		Reason:             lookingglassv1alpha.ReasonPending,
		Message:            "Placement is in progress.",
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
	return true
}

// Terminate ends a session with a terminal reason at now. It does nothing to
// a session that is already terminal, because a terminal reason is never
// replaced. It reports whether it changed the status.
func Terminate(s *lookingglassv1alpha.LookingGlassSession, reason, message string, now time.Time) bool {
	if IsTerminal(s) {
		return false
	}
	at := metav1.NewTime(now)
	s.Status.ObservedGeneration = s.Generation
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type:               lookingglassv1alpha.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.Generation,
		LastTransitionTime: at,
	})
	// Connection details are published only while the session is
	// SessionReady.
	s.Status.Connection = nil
	SetRetention(s, at)
	return true
}

// SetRetention sets FinishedAt, if unset, to finishedAt and derives
// ExpiresAt from it. The control plane owns ExpiresAt, so a value from
// anywhere else is overwritten. It reports whether it changed the status.
func SetRetention(s *lookingglassv1alpha.LookingGlassSession, finishedAt metav1.Time) bool {
	changed := false
	if s.Status.FinishedAt == nil {
		s.Status.FinishedAt = finishedAt.DeepCopy()
		changed = true
	}
	expiresAt := metav1.NewTime(s.Status.FinishedAt.Add(Retention))
	if s.Status.ExpiresAt == nil || !s.Status.ExpiresAt.Equal(&expiresAt) {
		s.Status.ExpiresAt = &expiresAt
		changed = true
	}
	return changed
}

// rank orders Ready reasons so status never moves backwards: Pending, then
// SessionReady, then Connected, then any terminal reason.
func rank(c *metav1.Condition) int {
	switch {
	case c == nil:
		return 0
	case c.Status == metav1.ConditionFalse:
		return 3
	case c.Status == metav1.ConditionTrue && c.Reason == lookingglassv1alpha.ReasonConnected:
		return 2
	case c.Status == metav1.ConditionTrue && c.Reason == lookingglassv1alpha.ReasonSessionReady:
		return 1
	default:
		return 0
	}
}

// terminalReasons are the Ready=False reasons a cell may report.
var terminalReasons = map[string]bool{
	lookingglassv1alpha.ReasonSucceeded:           true,
	lookingglassv1alpha.ReasonPartialResults:      true,
	lookingglassv1alpha.ReasonExecutionFailed:     true,
	lookingglassv1alpha.ReasonNoNodesAvailable:    true,
	lookingglassv1alpha.ReasonOutputLimitExceeded: true,
	lookingglassv1alpha.ReasonUnavailable:         true,
	lookingglassv1alpha.ReasonRefused:             true,
	lookingglassv1alpha.ReasonThrottled:           true,
	lookingglassv1alpha.ReasonNotConnected:        true,
	lookingglassv1alpha.ReasonDeadlineExceeded:    true,
	lookingglassv1alpha.ReasonRevoked:             true,
	lookingglassv1alpha.ReasonDisconnected:        true,
	lookingglassv1alpha.ReasonClosedByUser:        true,
	lookingglassv1alpha.ReasonAgentLost:           true,
	lookingglassv1alpha.ReasonBackendError:        true,
}

// validateReport checks the Ready condition a cell reported against the
// contract's reason set. SessionReady must carry the connection details and
// the connection deadline, so the AgentLost backstop always has an anchor.
func validateReport(c *metav1.Condition, in *lookingglassv1alpha.LookingGlassSessionStatus) error {
	switch c.Status {
	case metav1.ConditionUnknown:
		if c.Reason == lookingglassv1alpha.ReasonPending {
			return nil
		}
	case metav1.ConditionTrue:
		switch c.Reason {
		case lookingglassv1alpha.ReasonConnected:
			return nil
		case lookingglassv1alpha.ReasonSessionReady:
			if in.ConnectBefore == nil || in.Connection == nil {
				return fmt.Errorf("%w: SessionReady without connectBefore and connection", ErrInvalidStatus)
			}
			return nil
		}
	case metav1.ConditionFalse:
		if terminalReasons[c.Reason] {
			return nil
		}
	}
	return fmt.Errorf("%w: Ready=%s with reason %q", ErrInvalidStatus, c.Status, c.Reason)
}

// clamp bounds t to [lo, hi].
func clamp(t, lo, hi time.Time) time.Time {
	if t.Before(lo) {
		return lo
	}
	if t.After(hi) {
		return hi
	}
	return t
}

// MergeCellStatus folds status that cell reported for its copy of the
// session into s, the project's session. Status reflection (#8) calls it.
//
// It returns ErrNotBound if s has no cell, and ErrWrongCell if cell is not the
// bound cell or the reported status names another cell. In either case s is
// unchanged: the cell binding comes only from placement, never from cell
// data. It returns ErrInvalidStatus for a Ready condition outside the
// contract's reason set. Once s is terminal, or when the report would move
// the Ready reason backwards (for example a delayed Connected after a
// terminal reason), the report is ignored.
//
// Cell data is untrusted, so the times it carries are bounded by the
// control plane's clock: the transition time is now, the first connectBefore
// and startedAt win and cannot lie beyond the windows the contract allows,
// and finishedAt falls between creation and now. Connection details are kept
// only while the merged reason is SessionReady. It reports whether s changed.
func MergeCellStatus(s *lookingglassv1alpha.LookingGlassSession, cell string, in *lookingglassv1alpha.LookingGlassSessionStatus, now time.Time) (bool, error) {
	bound := s.Status.Cell
	if bound == "" {
		return false, ErrNotBound
	}
	if cell != bound || (in.Cell != "" && in.Cell != bound) {
		return false, ErrWrongCell
	}
	incoming := meta.FindStatusCondition(in.Conditions, lookingglassv1alpha.ConditionReady)
	if incoming == nil {
		return false, nil
	}
	if err := validateReport(incoming, in); err != nil {
		return false, err
	}
	if IsTerminal(s) {
		return false, nil
	}
	current := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	if rank(incoming) < rank(current) {
		return false, nil
	}

	before := s.Status.DeepCopy()
	created := s.CreationTimestamp.Time
	cond := metav1.Condition{
		Type:               lookingglassv1alpha.ConditionReady,
		Status:             incoming.Status,
		Reason:             incoming.Reason,
		Message:            incoming.Message,
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.NewTime(now),
	}
	s.Status.ObservedGeneration = s.Generation
	meta.SetStatusCondition(&s.Status.Conditions, cond)

	if s.Status.ConnectBefore == nil && in.ConnectBefore != nil {
		cb := metav1.NewTime(clamp(in.ConnectBefore.Time, created, now.Add(ConnectionWindow)))
		s.Status.ConnectBefore = &cb
	}
	if s.Status.StartedAt == nil && in.StartedAt != nil {
		started := metav1.NewTime(clamp(in.StartedAt.Time, created, now))
		s.Status.StartedAt = &started
	}
	if in.Coverage != nil {
		s.Status.Coverage = in.Coverage.DeepCopy()
	}
	if in.RetryAfter != nil {
		s.Status.RetryAfter = in.RetryAfter.DeepCopy()
	}
	if cond.Status == metav1.ConditionTrue && cond.Reason == lookingglassv1alpha.ReasonSessionReady {
		s.Status.Connection = in.Connection.DeepCopy()
	} else {
		s.Status.Connection = nil
	}
	if cond.Status == metav1.ConditionFalse {
		finishedAt := now
		if in.FinishedAt != nil {
			finishedAt = clamp(in.FinishedAt.Time, created, now)
		}
		SetRetention(s, metav1.NewTime(finishedAt))
	}
	return !equality.Semantic.DeepEqual(before, &s.Status), nil
}
