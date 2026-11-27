package session

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lookingglassv1alpha "go.datum.net/looking-glass/api/v1alpha"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func ready(status metav1.ConditionStatus, reason string) []metav1.Condition {
	return []metav1.Condition{{
		Type:               lookingglassv1alpha.ConditionReady,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(t0),
	}}
}

func boundSession(conds []metav1.Condition) *lookingglassv1alpha.LookingGlassSession {
	return &lookingglassv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, CreationTimestamp: metav1.NewTime(t0)},
		Status:     lookingglassv1alpha.LookingGlassSessionStatus{Cell: "cell-1", Conditions: conds},
	}
}

func reason(s *lookingglassv1alpha.LookingGlassSession) string {
	if c := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func connection() *lookingglassv1alpha.SessionConnection {
	return &lookingglassv1alpha.SessionConnection{
		EndpointID: "0000000000000000000000000000000000000000000000000000000000000001",
		Target:     "gateway.example.com:443",
	}
}

func TestDeadlines(t *testing.T) {
	s := boundSession(nil)
	if got, want := PlacementDeadline(s), t0.Add(30*time.Second); !got.Equal(want) {
		t.Errorf("PlacementDeadline = %v; want %v", got, want)
	}
	if _, ok := AgentLostDeadline(s); ok {
		t.Error("AgentLostDeadline is set before connectBefore")
	}
	connectBefore := metav1.NewTime(t0.Add(time.Minute))
	s.Status.ConnectBefore = &connectBefore
	got, ok := AgentLostDeadline(s)
	if want := connectBefore.Add(150 * time.Second); !ok || !got.Equal(want) {
		t.Errorf("AgentLostDeadline = %v, %v; want %v", got, ok, want)
	}
}

func TestSetPendingOnce(t *testing.T) {
	s := boundSession(nil)
	if !SetPending(s, t0) {
		t.Fatal("SetPending did not write a new session")
	}
	c := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	if c.Status != metav1.ConditionUnknown || c.Reason != lookingglassv1alpha.ReasonPending || c.ObservedGeneration != 1 {
		t.Fatalf("Ready = %+v; want Unknown/Pending at generation 1", c)
	}
	if SetPending(s, t0.Add(time.Second)) {
		t.Fatal("SetPending rewrote an existing Ready condition")
	}
}

func TestTerminateNeverReplacesTerminalReason(t *testing.T) {
	s := boundSession(ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady))
	s.Status.Connection = connection()
	if !Terminate(s, lookingglassv1alpha.ReasonUnavailable, "", t0) {
		t.Fatal("Terminate did not end a live session")
	}
	if s.Status.Connection != nil {
		t.Error("Terminate kept connection details")
	}
	if !s.Status.FinishedAt.Time.Equal(t0) || !s.Status.ExpiresAt.Time.Equal(t0.Add(15*time.Minute)) {
		t.Errorf("FinishedAt, ExpiresAt = %v, %v; want %v and 15 m later", s.Status.FinishedAt, s.Status.ExpiresAt, t0)
	}
	if Terminate(s, lookingglassv1alpha.ReasonAgentLost, "", t0.Add(time.Minute)) {
		t.Fatal("Terminate replaced a terminal reason")
	}
	if got := reason(s); got != lookingglassv1alpha.ReasonUnavailable {
		t.Errorf("reason = %q; want Unavailable", got)
	}
}

func TestMergeCellStatusGuards(t *testing.T) {
	tests := []struct {
		name    string
		session *lookingglassv1alpha.LookingGlassSession
		cell    string
		in      lookingglassv1alpha.LookingGlassSessionStatus
		wantErr error
		want    string
	}{
		{
			name:    "unbound session adopts no cell",
			session: &lookingglassv1alpha.LookingGlassSession{Status: lookingglassv1alpha.LookingGlassSessionStatus{Conditions: ready(metav1.ConditionUnknown, lookingglassv1alpha.ReasonPending)}},
			cell:    "cell-1",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Cell: "cell-1", Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady)},
			wantErr: ErrNotBound,
			want:    lookingglassv1alpha.ReasonPending,
		},
		{
			name:    "status from another cell",
			session: boundSession(ready(metav1.ConditionUnknown, lookingglassv1alpha.ReasonPending)),
			cell:    "cell-2",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady)},
			wantErr: ErrWrongCell,
			want:    lookingglassv1alpha.ReasonPending,
		},
		{
			name:    "bound cell naming another cell",
			session: boundSession(ready(metav1.ConditionUnknown, lookingglassv1alpha.ReasonPending)),
			cell:    "cell-1",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Cell: "cell-2", Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady)},
			wantErr: ErrWrongCell,
			want:    lookingglassv1alpha.ReasonPending,
		},
		{
			name:    "delayed non-terminal update after a terminal reason",
			session: boundSession(ready(metav1.ConditionFalse, lookingglassv1alpha.ReasonAgentLost)),
			cell:    "cell-1",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonConnected)},
			want:    lookingglassv1alpha.ReasonAgentLost,
		},
		{
			name:    "later terminal reason after a terminal reason",
			session: boundSession(ready(metav1.ConditionFalse, lookingglassv1alpha.ReasonAgentLost)),
			cell:    "cell-1",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Conditions: ready(metav1.ConditionFalse, lookingglassv1alpha.ReasonSucceeded)},
			want:    lookingglassv1alpha.ReasonAgentLost,
		},
		{
			name:    "out-of-order SessionReady after Connected",
			session: boundSession(ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonConnected)),
			cell:    "cell-1",
			in:      lookingglassv1alpha.LookingGlassSessionStatus{Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady)},
			want:    lookingglassv1alpha.ReasonConnected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.session.DeepCopy()
			changed, err := MergeCellStatus(tt.session, tt.cell, &tt.in, t0)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v; want %v", err, tt.wantErr)
			}
			if changed {
				t.Error("MergeCellStatus reported a change")
			}
			if got := reason(tt.session); got != tt.want {
				t.Errorf("reason = %q; want %q", got, tt.want)
			}
			if tt.session.Status.Cell != before.Status.Cell {
				t.Errorf("cell = %q; want %q", tt.session.Status.Cell, before.Status.Cell)
			}
		})
	}
}

func TestMergeCellStatusFromBoundCell(t *testing.T) {
	s := boundSession(ready(metav1.ConditionUnknown, lookingglassv1alpha.ReasonPending))
	connectBefore := metav1.NewTime(t0.Add(time.Minute))
	changed, err := MergeCellStatus(s, "cell-1", &lookingglassv1alpha.LookingGlassSessionStatus{
		Cell:          "cell-1",
		Conditions:    ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady),
		Connection:    connection(),
		ConnectBefore: &connectBefore,
	}, t0)
	if err != nil || !changed {
		t.Fatalf("SessionReady: changed, err = %v, %v; want true, nil", changed, err)
	}
	if reason(s) != lookingglassv1alpha.ReasonSessionReady || s.Status.Connection == nil || !s.Status.ConnectBefore.Equal(&connectBefore) {
		t.Fatalf("status after SessionReady = %+v", s.Status)
	}

	cellFinished := metav1.NewTime(t0.Add(2 * time.Minute))
	bogusExpiry := metav1.NewTime(t0.Add(48 * time.Hour))
	changed, err = MergeCellStatus(s, "cell-1", &lookingglassv1alpha.LookingGlassSessionStatus{
		Conditions: ready(metav1.ConditionFalse, lookingglassv1alpha.ReasonSucceeded),
		Connection: connection(),
		FinishedAt: &cellFinished,
		ExpiresAt:  &bogusExpiry,
		Coverage:   &lookingglassv1alpha.Coverage{Complete: true, Expected: 2, Selected: 2, Successful: 2},
	}, t0.Add(3*time.Minute))
	if err != nil || !changed {
		t.Fatalf("Succeeded: changed, err = %v, %v; want true, nil", changed, err)
	}
	if reason(s) != lookingglassv1alpha.ReasonSucceeded {
		t.Errorf("reason = %q; want Succeeded", reason(s))
	}
	if s.Status.Connection != nil {
		t.Error("connection details kept after the session ended")
	}
	if !s.Status.FinishedAt.Equal(&cellFinished) || !s.Status.ExpiresAt.Time.Equal(cellFinished.Add(15*time.Minute)) {
		t.Errorf("FinishedAt, ExpiresAt = %v, %v; want %v and 15 m later", s.Status.FinishedAt, s.Status.ExpiresAt, cellFinished)
	}
	if s.Status.Coverage == nil || s.Status.Coverage.Successful != 2 {
		t.Errorf("coverage = %+v; want the cell's counts", s.Status.Coverage)
	}
}
