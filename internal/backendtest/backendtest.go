// Package backendtest is the conformance suite for implementations of the
// backend API (lookingglass.backend.v1.DiagnosticService). Every backend runs
// it, so the gateway can rely on the stream grammar, the coverage arithmetic
// and the refusals before the snapshot that docs/api/v1alpha-contract.md
// defines, whatever routers sit behind the backend.
package backendtest

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
)

// Fixture describes what the backend under test serves.
type Fixture struct {
	// Cell is the cell the backend serves.
	Cell string
	// Location is a location the backend serves.
	Location string
	// Routers lists every router serving Location, all eligible.
	Routers []string
	// Queries are diagnostics the backend serves at Location. The suite runs
	// each one and checks its stream.
	Queries []*backendv1.Query
}

// DefaultQueries returns one query per diagnostic for IPv4, with
// documentation-range targets.
func DefaultQueries() []*backendv1.Query {
	v4 := backendv1.AddressFamily_ADDRESS_FAMILY_IPV4
	return []*backendv1.Query{
		{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP, AddressFamily: v4, Target: "198.51.100.7"},
		{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY, AddressFamily: v4},
		{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING, AddressFamily: v4, Target: "198.51.100.7"},
		{Type: backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE, AddressFamily: v4, Target: "198.51.100.7"},
	}
}

// Request returns a valid request for q at the fixture's location, with a
// fresh request ID and a deadline 30 s from now.
func (f Fixture) Request(q *backendv1.Query) *backendv1.RunDiagnosticRequest {
	return &backendv1.RunDiagnosticRequest{
		ApiMajor:  backendv1.APIMajor,
		Cell:      f.Cell,
		Location:  f.Location,
		ProjectId: "project-conformance",
		RequestId: uuid.NewString(),
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second)),
		Query:     q,
	}
}

// Run runs the conformance suite against client.
func Run(t *testing.T, client backendv1.DiagnosticServiceClient, f Fixture) {
	t.Helper()
	t.Run("Capabilities", func(t *testing.T) { testCapabilities(t, client, f) })
	for _, q := range f.Queries {
		t.Run("Stream/"+q.GetType().String(), func(t *testing.T) {
			events, err := Collect(t.Context(), client, f.Request(q))
			if err != nil {
				t.Fatalf("RunDiagnostic: %v", err)
			}
			CheckStream(t, f, q, events)
		})
	}
	t.Run("Refusals", func(t *testing.T) { testRefusals(t, client, f) })
}

// Collect runs req and returns every event the stream delivered before it
// ended. The error is the stream's terminal error, nil after a clean end.
func Collect(ctx context.Context, client backendv1.DiagnosticServiceClient, req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
	stream, err := client.RunDiagnostic(ctx, req)
	if err != nil {
		return nil, err
	}
	var events []*backendv1.DiagnosticEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

// CheckStream checks one complete stream for q against the contract: one
// snapshot first, one outcome per selected router, one complete last, with
// coverage counts that add up.
func CheckStream(t testing.TB, f Fixture, q *backendv1.Query, events []*backendv1.DiagnosticEvent) {
	t.Helper()
	if len(events) < 2 {
		t.Fatalf("stream has %d events, want at least a snapshot and a complete", len(events))
	}
	for i, ev := range events {
		if ev.GetSequence() != uint64(i+1) {
			t.Errorf("event %d has sequence %d, want %d", i, ev.GetSequence(), i+1)
		}
		if ev.GetRequestId() != events[0].GetRequestId() || ev.GetRequestId() == "" {
			t.Errorf("event %d has request_id %q, want the request's ID on every event", i, ev.GetRequestId())
		}
	}

	snap := events[0].GetSnapshot()
	if snap == nil {
		t.Fatalf("first event is %T, want a snapshot", events[0].GetEvent())
	}
	complete := events[len(events)-1].GetComplete()
	if complete == nil {
		t.Fatalf("last event is %T, want a complete", events[len(events)-1].GetEvent())
	}

	var names []string
	selected := map[string]bool{}
	var omitted, ineligible uint32
	for _, r := range snap.GetRouters() {
		names = append(names, r.GetRouter())
		if r.GetSelected() {
			selected[r.GetRouter()] = true
			if r.GetOmittedReason() != "" {
				t.Errorf("selected router %s has omitted_reason %q", r.GetRouter(), r.GetOmittedReason())
			}
			continue
		}
		omitted++
		if r.GetOmittedReason() == backendv1.OmittedIneligible {
			ineligible++
		}
		if !slices.Contains([]string{backendv1.OmittedNotRequested, backendv1.OmittedIneligible}, r.GetOmittedReason()) {
			t.Errorf("omitted router %s has omitted_reason %q", r.GetRouter(), r.GetOmittedReason())
		}
	}
	slices.Sort(names)
	want := slices.Sorted(slices.Values(f.Routers))
	if !slices.Equal(names, want) {
		t.Errorf("snapshot routers = %v, want %v", names, want)
	}

	seen := map[string]bool{}
	var successful, failed, truncated uint32
	for i, ev := range events[1 : len(events)-1] {
		o := ev.GetOutcome()
		if o == nil {
			t.Errorf("event %d is %T, want an outcome between the snapshot and the complete", i+1, ev.GetEvent())
			continue
		}
		if !selected[o.GetRouter()] || seen[o.GetRouter()] {
			t.Errorf("outcome for %s is not for a selected router, or repeats one", o.GetRouter())
		}
		seen[o.GetRouter()] = true
		if o.GetCompletedAt() == nil {
			t.Errorf("outcome for %s has no completed_at", o.GetRouter())
		}
		switch {
		case o.GetObservation() != nil:
			successful++
			if o.GetObservation().GetTruncated() {
				truncated++
			}
			checkObservation(t, q, o.GetRouter(), o.GetObservation())
		case o.GetFailure() != nil:
			failed++
			if o.GetFailure().GetReason() == "" {
				t.Errorf("failure for %s has no reason", o.GetRouter())
			}
			if len(o.GetFailure().GetMessage()) > 256 {
				t.Errorf("failure message for %s is %d bytes, want at most 256", o.GetRouter(), len(o.GetFailure().GetMessage()))
			}
		default:
			t.Errorf("outcome for %s has neither an observation nor a failure", o.GetRouter())
		}
	}
	if len(seen) != len(selected) {
		t.Errorf("got outcomes for %d routers, want one for each of the %d selected", len(seen), len(selected))
	}

	if complete.GetSnapshotId() != snap.GetSnapshotId() {
		t.Errorf("complete.snapshot_id = %q, want the snapshot's %q", complete.GetSnapshotId(), snap.GetSnapshotId())
	}
	got := [5]uint32{complete.GetExpected(), complete.GetSuccessful(), complete.GetFailed(), complete.GetOmitted(), complete.GetTruncated()}
	wantCounts := [5]uint32{uint32(len(snap.GetRouters())), successful, failed, omitted, truncated}
	if got != wantCounts {
		t.Errorf("complete counts (expected, successful, failed, omitted, truncated) = %v, want %v", got, wantCounts)
	}
	if complete.GetExpected() != complete.GetSuccessful()+complete.GetFailed()+complete.GetOmitted() {
		t.Errorf("expected %d != successful + failed + omitted", complete.GetExpected())
	}
	if complete.GetFinishedAt() == nil {
		t.Error("complete has no finished_at")
	}
	if want := terminalReason(complete, ineligible); complete.GetReason() != want {
		t.Errorf("complete.reason = %q, want %q for these counts", complete.GetReason(), want)
	}
}

// terminalReason is the reason a complete stream's counts imply, when no
// deadline or cancellation stopped it. An ineligible router makes a result
// partial; a router the request did not ask for does not.
func terminalReason(c *backendv1.Complete, ineligible uint32) string {
	selected := c.GetSuccessful() + c.GetFailed()
	switch {
	case selected == 0:
		return backendv1.CompleteNoNodesAvailable
	case c.GetSuccessful() == 0:
		return backendv1.CompleteExecutionFailed
	case c.GetFailed() > 0 || c.GetTruncated() > 0 || ineligible > 0:
		return backendv1.CompletePartialResults
	default:
		return backendv1.CompleteSucceeded
	}
}

func checkObservation(t testing.TB, q *backendv1.Query, router string, obs *backendv1.Observation) {
	t.Helper()
	if obs.GetSampleTime() == nil {
		t.Errorf("observation for %s has no sample_time", router)
	}
	var ok bool
	switch q.GetType() {
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_ROUTE_LOOKUP:
		ok = obs.GetRoutes() != nil
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_BGP_SUMMARY:
		ok = obs.GetBgpSummary() != nil
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING:
		ok = obs.GetPing() != nil
	case backendv1.DiagnosticType_DIAGNOSTIC_TYPE_TRACEROUTE:
		ok = obs.GetTraceroute() != nil
	}
	if !ok {
		t.Errorf("observation for %s carries %T, want the result for %s", router, obs.GetResult(), q.GetType())
	}
}

func testCapabilities(t *testing.T, client backendv1.DiagnosticServiceClient, f Fixture) {
	caps, err := client.GetCapabilities(t.Context(), &backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: f.Cell})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.GetCell() != f.Cell || caps.GetApiMajor() != backendv1.APIMajor || !caps.GetEnabled() {
		t.Errorf("capabilities cell, api_major, enabled = %q, %d, %v; want %q, %d, true",
			caps.GetCell(), caps.GetApiMajor(), caps.GetEnabled(), f.Cell, backendv1.APIMajor)
	}
	i := slices.IndexFunc(caps.GetLocations(), func(l *backendv1.LocationCapabilities) bool { return l.GetLocation() == f.Location })
	if i < 0 {
		t.Fatalf("capabilities do not list location %q", f.Location)
	}
	loc := caps.GetLocations()[i]
	if loc.GetEligibleRouters() != uint32(len(f.Routers)) {
		t.Errorf("eligible_routers = %d, want %d", loc.GetEligibleRouters(), len(f.Routers))
	}
	for _, q := range f.Queries {
		served := slices.ContainsFunc(loc.GetOperations(), func(op *backendv1.Operation) bool {
			return op.GetType() == q.GetType() && slices.Contains(op.GetAddressFamilies(), q.GetAddressFamily())
		})
		if !served {
			t.Errorf("capabilities do not offer %s for %s, which the fixture runs", q.GetType(), q.GetAddressFamily())
		}
	}
	if caps.GetLimits().GetMaxRequestLifetime() == nil || caps.GetGeneratedAt() == nil {
		t.Error("capabilities lack max_request_lifetime or generated_at")
	}

	for name, req := range map[string]struct {
		req    *backendv1.GetCapabilitiesRequest
		reason string
	}{
		"unsupported api major": {&backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor + 1, Cell: f.Cell}, backendv1.ReasonVersionUnsupported},
		"another cell":          {&backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: f.Cell + "-other"}, backendv1.ReasonUnauthorized},
	} {
		_, err := client.GetCapabilities(t.Context(), req.req)
		if got, _ := backendv1.RefusalReason(err); got != req.reason {
			t.Errorf("GetCapabilities with %s: reason %q (%v), want %q", name, got, err, req.reason)
		}
	}
}

func testRefusals(t *testing.T, client backendv1.DiagnosticServiceClient, f Fixture) {
	tests := []struct {
		name   string
		mutate func(*backendv1.RunDiagnosticRequest)
		reason string
	}{
		{"unsupported api major", func(r *backendv1.RunDiagnosticRequest) { r.ApiMajor++ }, backendv1.ReasonVersionUnsupported},
		{"another cell", func(r *backendv1.RunDiagnosticRequest) { r.Cell += "-other" }, backendv1.ReasonUnauthorized},
		{"unserved location", func(r *backendv1.RunDiagnosticRequest) { r.Location += "-unserved" }, backendv1.ReasonLocationNotServed},
		{"past deadline", func(r *backendv1.RunDiagnosticRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
		}, backendv1.ReasonDeadlineExceeded},
		{"no deadline", func(r *backendv1.RunDiagnosticRequest) { r.ExpiresAt = nil }, backendv1.ReasonInvalidQuery},
		{"deadline beyond the lifetime", func(r *backendv1.RunDiagnosticRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().Add(time.Hour))
		}, backendv1.ReasonInvalidQuery},
		{"malformed request id", func(r *backendv1.RunDiagnosticRequest) { r.RequestId = "not-a-uuid" }, backendv1.ReasonInvalidQuery},
		{"malformed project id", func(r *backendv1.RunDiagnosticRequest) { r.ProjectId = "" }, backendv1.ReasonInvalidQuery},
		{"no query", func(r *backendv1.RunDiagnosticRequest) { r.Query = nil }, backendv1.ReasonQueryTypeUnavailable},
		{"no target", func(r *backendv1.RunDiagnosticRequest) { r.Query.Target = "" }, backendv1.ReasonInvalidQuery},
		{"family mismatch", func(r *backendv1.RunDiagnosticRequest) {
			r.Query.AddressFamily = backendv1.AddressFamily_ADDRESS_FAMILY_IPV6
		}, backendv1.ReasonInvalidQuery},
		{"target a name", func(r *backendv1.RunDiagnosticRequest) { r.Query.Target = "example.com" }, backendv1.ReasonInvalidQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := f.Request(&backendv1.Query{
				Type:          backendv1.DiagnosticType_DIAGNOSTIC_TYPE_PING,
				AddressFamily: backendv1.AddressFamily_ADDRESS_FAMILY_IPV4,
				Target:        "198.51.100.7",
			})
			tt.mutate(req)
			events, err := Collect(t.Context(), client, req)
			if len(events) != 0 {
				t.Errorf("got %d events before the refusal, want none", len(events))
			}
			if got, _ := backendv1.RefusalReason(err); got != tt.reason {
				t.Errorf("refusal reason %q (%v), want %q", got, err, tt.reason)
			}
		})
	}
}
