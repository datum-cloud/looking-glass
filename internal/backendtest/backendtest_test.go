package backendtest

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
)

// recorder captures failures instead of failing the enclosing test.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// check runs CheckStream in its own goroutine, so a Fatalf ends only that
// goroutine, and returns the recorded failures.
func check(t *testing.T, events []*backendv1.DiagnosticEvent) []string {
	r := &recorder{TB: t}
	f := Fixture{Cell: "cell-a", Location: "loc", Routers: []string{"r1", "r2"}}
	q := &backendv1.Query{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING}
	done := make(chan struct{})
	go func() {
		defer close(done)
		CheckStream(r, f, q, events)
	}()
	<-done
	return r.errors
}

// goodStream is a valid stream: both routers selected, r1 answers, r2 fails.
func goodStream() []*backendv1.DiagnosticEvent {
	now := timestamppb.Now()
	evs := []*backendv1.DiagnosticEvent{
		{Event: &backendv1.DiagnosticEvent_Snapshot{Snapshot: &backendv1.Snapshot{
			SnapshotId: "s",
			Routers:    []*backendv1.SnapshotRouter{{Router: "r1", Selected: true}, {Router: "r2", Selected: true}},
		}}},
		{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: &backendv1.RouterOutcome{
			Router: "r1", CompletedAt: now,
			Result: &backendv1.RouterOutcome_Observation{Observation: &backendv1.Observation{
				SampleTime: now, Result: &backendv1.Observation_Ping{Ping: &backendv1.PingResult{}},
			}},
		}}},
		{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: &backendv1.RouterOutcome{
			Router: "r2", CompletedAt: now,
			Result: &backendv1.RouterOutcome_Failure{Failure: &backendv1.Failure{Reason: backendv1.ReasonRouterUnreachable}},
		}}},
		{Event: &backendv1.DiagnosticEvent_Complete{Complete: &backendv1.Complete{
			SnapshotId: "s", FinishedAt: now, Expected: 2, Successful: 1, Failed: 1,
			Reason: backendv1.CompletePartialResults,
		}}},
	}
	for i, ev := range evs {
		ev.RequestId, ev.Sequence = "req", uint64(i+1)
	}
	return evs
}

func TestCheckStreamAcceptsValidStream(t *testing.T) {
	if errs := check(t, goodStream()); len(errs) != 0 {
		t.Errorf("valid stream reported: %v", errs)
	}
}

func TestCheckStreamReportsViolations(t *testing.T) {
	tests := map[string]struct {
		mutate func([]*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent
		want   string
	}{
		"no snapshot first": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[0], e[1] = e[1], e[0]
			return e
		}, "want a snapshot"},
		"no complete": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent { return e[:3] }, "want a complete"},
		"sequence gap": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[2].Sequence = 7
			return e
		}, "sequence 7"},
		"missing outcome": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			return append(e[:2], e[3])
		}, "outcomes for 1 routers"},
		"outcome for unselected router": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[2].GetOutcome().Router = "r9"
			return e
		}, "not for a selected router"},
		"counts do not add up": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[3].GetComplete().Successful = 2
			return e
		}, "complete counts"},
		"wrong terminal reason": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[3].GetComplete().Reason = backendv1.CompleteSucceeded
			return e
		}, "want \"PartialResults\""},
		"wrong result type": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[1].GetOutcome().GetObservation().Result = &backendv1.Observation_Traceroute{Traceroute: &backendv1.TracerouteResult{}}
			return e
		}, "want the result for"},
		"snapshot router set differs": {func(e []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
			e[0].GetSnapshot().Routers[1].Router = "r3"
			return e
		}, "snapshot routers"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			events := tc.mutate(clone(goodStream()))
			errs := check(t, events)
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Errorf("CheckStream reported %q, want a failure mentioning %q", errs, tc.want)
			}
		})
	}
}

func clone(evs []*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
	out := make([]*backendv1.DiagnosticEvent, len(evs))
	for i, ev := range evs {
		out[i] = proto.CloneOf(ev)
	}
	return out
}
