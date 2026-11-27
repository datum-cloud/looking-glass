package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

var update = flag.Bool("update", false, "rewrite the golden frame files")

var diagnostics = map[string]v1alpha.Diagnostic{
	"route-lookup-v4": {Type: v1alpha.DiagnosticRouteLookup, RouteLookup: &v1alpha.RouteLookupDiagnostic{Destination: "198.51.100.7"}},
	"route-lookup-v6": {Type: v1alpha.DiagnosticRouteLookup, RouteLookup: &v1alpha.RouteLookupDiagnostic{Destination: "2001:db8::/32"}},
	"bgp-summary-v4":  {Type: v1alpha.DiagnosticBGPSummary, BGPSummary: &v1alpha.BGPSummaryDiagnostic{AddressFamily: v1alpha.AddressFamilyIPv4}},
	"bgp-summary-v6":  {Type: v1alpha.DiagnosticBGPSummary, BGPSummary: &v1alpha.BGPSummaryDiagnostic{AddressFamily: v1alpha.AddressFamilyIPv6}},
	"ping-v4":         pingDiagnostic("198.51.100.7"),
	"ping-v6":         pingDiagnostic("2001:db8::7"),
	"traceroute-v4":   {Type: v1alpha.DiagnosticTraceroute, Traceroute: &v1alpha.TracerouteDiagnostic{Destination: "198.51.100.7"}},
	"traceroute-v6":   {Type: v1alpha.DiagnosticTraceroute, Traceroute: &v1alpha.TracerouteDiagnostic{Destination: "2001:DB8::7"}},
}

// timeKeys are the frame fields that hold wall-clock times. The golden files
// hold a placeholder for them.
var timeKeys = map[string]bool{"deadline": true, "finishedAt": true, "completedAt": true, "sampleTime": true}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if timeKeys[k] {
				if _, err := time.Parse(time.RFC3339Nano, val.(string)); err != nil {
					panic("bad time in " + k)
				}
				x[k] = "<time>"
				continue
			}
			x[k] = normalize(val)
		}
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
	}
	return v
}

// TestEachDiagnosticStreamsTypedFrames runs every diagnostic in both
// families against the fake and compares the frames with golden files.
func TestEachDiagnosticStreamsTypedFrames(t *testing.T) {
	for name, d := range diagnostics {
		t.Run(name, func(t *testing.T) {
			b := startBackend(t, fakeOptions())
			s := session(d)
			out := newSink()
			res := newAdapter(t, b.client).Run(t.Context(), s, out)

			if res.Reason != v1alpha.ReasonSucceeded || !res.EndSent {
				t.Fatalf("result = %+v, want Succeeded with the end frame sent", res)
			}
			if c := res.Coverage; c == nil || !c.Complete || c.Expected != 3 || c.Successful != 3 {
				t.Fatalf("coverage = %+v, want complete with 3 successful", c)
			}
			if n := b.runCalls.Load(); n != 1 {
				t.Fatalf("backend calls = %d, want 1", n)
			}
			checkRequest(t, b.requests[0], s)

			frames := out.decoded(t)
			want := []string{FrameHello, FrameSnapshot, FrameOutcome, FrameOutcome, FrameOutcome, FrameEnd}
			if got := frameTypes(frames); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("frames = %v, want %v", got, want)
			}
			for i, f := range frames {
				if seq := f["seq"].(float64); int(seq) != i+1 {
					t.Fatalf("frame %d has seq %v", i, seq)
				}
				if f["type"] != FrameOutcome {
					continue
				}
				router, _ := f["router"].(string)
				obs, _ := f["observation"].(map[string]any)
				if !strings.HasPrefix(router, "r-") || len(router) != 18 || obs == nil || obs["sampleTime"] == "" {
					t.Fatalf("outcome %d lacks an opaque router or a sample time: %v", i, f)
				}
			}

			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(normalize(toAny(frames))); err != nil {
				t.Fatal(err)
			}
			got := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
			golden := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, append(got, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			wantBytes, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(wantBytes) != string(got)+"\n" {
				t.Fatalf("frames differ from %s:\n%s", golden, got)
			}
		})
	}
}

func toAny(frames []map[string]any) []any {
	out := make([]any, len(frames))
	for i, f := range frames {
		out[i] = f
	}
	return out
}

// checkRequest checks the one request against the contract's request table.
func checkRequest(t *testing.T, req *backendv1.RunDiagnosticRequest, s Session) {
	t.Helper()
	id, err := uuid.Parse(req.GetRequestId())
	switch {
	case req.GetApiMajor() != 1, req.GetCell() != s.Cell, req.GetLocation() != s.Location:
		t.Fatalf("request names api %d, cell %q, location %q", req.GetApiMajor(), req.GetCell(), req.GetLocation())
	case req.GetProjectId() != s.ProjectID:
		t.Fatalf("project_id = %q, want the session's project", req.GetProjectId())
	case err != nil || id.String() != req.GetRequestId():
		t.Fatalf("request_id %q is not a lowercase UUID", req.GetRequestId())
	case !req.GetExpiresAt().AsTime().Equal(s.StartedAt.Add(90 * time.Second)):
		t.Fatalf("expires_at = %s, want startedAt + 90s", req.GetExpiresAt().AsTime())
	}
}

func TestRouterIDIsOpaqueAndStable(t *testing.T) {
	id := RouterID("example-location", "router-a")
	if id != RouterID("example-location", "router-a") {
		t.Fatal("router ID is not stable")
	}
	if !strings.HasPrefix(id, "r-") || len(id) != 18 || strings.Contains(id, "router") {
		t.Fatalf("router ID %q is not r- and 16 hex characters", id)
	}
	if id == RouterID("other-location", "router-a") {
		t.Fatal("router ID does not depend on the location")
	}
}

// TestObservationsAreNotFailures streams packet loss, a silent traceroute hop
// and an empty route lookup, and expects each as an observation.
func TestObservationsAreNotFailures(t *testing.T) {
	cases := map[string]struct {
		d   v1alpha.Diagnostic
		obs *backendv1.Observation
	}{
		"packet loss": {pingDiagnostic("198.51.100.7"), &backendv1.Observation{Result: &backendv1.Observation_Ping{Ping: &backendv1.PingResult{
			Sent: 3, Received: 0, Replies: []*backendv1.ProbeReply{{Sequence: 1, Timeout: true}},
		}}}},
		"silent hop": {diagnostics["traceroute-v4"], &backendv1.Observation{Result: &backendv1.Observation_Traceroute{Traceroute: &backendv1.TracerouteResult{
			Hops: []*backendv1.Hop{{Ttl: 1, Probes: []*backendv1.ProbeReply{{Sequence: 1, Timeout: true}}}},
		}}}},
		"empty route lookup": {diagnostics["route-lookup-v4"], &backendv1.Observation{Result: &backendv1.Observation_Routes{Routes: &backendv1.RouteResult{
			LookupKind: backendv1.LookupKind_LOOKUP_KIND_LONGEST_MATCH,
		}}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := &scripted{script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
				return numbered(req, snapshotEvent("router-a"), observationEvent("router-a", tc.obs),
					completeEvent(&backendv1.Complete{Expected: 1, Successful: 1, Reason: backendv1.CompleteSucceeded})), nil
			}}
			out := newSink()
			res := newAdapter(t, client).Run(t.Context(), session(tc.d), out)
			if res.Reason != v1alpha.ReasonSucceeded {
				t.Fatalf("reason = %s, want Succeeded", res.Reason)
			}
			outcome := out.decoded(t)[2]
			if outcome["observation"] == nil || outcome["failure"] != nil {
				t.Fatalf("outcome = %v, want an observation", outcome)
			}
		})
	}
}

func TestFailureReasonMapping(t *testing.T) {
	want := map[string]string{
		backendv1.ReasonRouterUnreachable:      FailureUnavailable,
		backendv1.ReasonRouterUnavailable:      FailureUnavailable,
		backendv1.ReasonDestinationNotAllowed:  FailureRefused,
		backendv1.ReasonInvalidQuery:           FailureRefused,
		backendv1.ReasonTimeout:                FailureTimeout,
		backendv1.ReasonQueueTimeout:           FailureTimeout,
		backendv1.ReasonDeadlineExceeded:       FailureTimeout,
		backendv1.ReasonRateLimited:            FailureThrottled,
		backendv1.ReasonQueueFull:              FailureThrottled,
		backendv1.ReasonRouterBusy:             FailureThrottled,
		backendv1.ReasonQueryTypeUnavailable:   FailureUnsupported,
		backendv1.ReasonProbeSourceUnavailable: FailureUnsupported,
		backendv1.ReasonResponseTooLarge:       FailureResponseTooLarge,
		backendv1.ReasonCancelled:              FailureCancelled,
		backendv1.ReasonParseError:             FailureBackendError,
		backendv1.ReasonInternal:               FailureBackendError,
		"SomethingNew":                         FailureBackendError,
	}
	for backend, reason := range want {
		t.Run(backend, func(t *testing.T) {
			client := &scripted{script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
				return numbered(req, snapshotEvent("router-a"), failureEvent("router-a", backend),
					completeEvent(&backendv1.Complete{Expected: 1, Failed: 1, Reason: backendv1.CompleteExecutionFailed})), nil
			}}
			out := newSink()
			res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
			if res.Reason != v1alpha.ReasonExecutionFailed {
				t.Fatalf("reason = %s, want ExecutionFailed", res.Reason)
			}
			f := out.decoded(t)[2]["failure"].(map[string]any)
			if f["reason"] != reason || f["backendReason"] != backend {
				t.Fatalf("failure = %v, want reason %s and backendReason %s", f, reason, backend)
			}
		})
	}
}

func TestUnreachableRouterIsAPartialResult(t *testing.T) {
	opts := fakeOptions()
	opts.Unreachable = []string{"router-b"}
	b := startBackend(t, opts)
	res := newAdapter(t, b.client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), newSink())
	if res.Reason != v1alpha.ReasonPartialResults || res.Coverage.Failed != 1 || !res.Coverage.Complete {
		t.Fatalf("result = %+v, coverage %+v; want PartialResults with one failure", res, res.Coverage)
	}
}

// TestRefusalsBeforeTheSnapshot covers the contract's pre-snapshot table: the
// client gets hello then end, with no snapshot and no coverage.
func TestRefusalsBeforeTheSnapshot(t *testing.T) {
	want := map[string]string{
		backendv1.ReasonUnauthorized:           v1alpha.ReasonRefused,
		backendv1.ReasonVersionUnsupported:     v1alpha.ReasonRefused,
		backendv1.ReasonQueryTypeUnavailable:   v1alpha.ReasonRefused,
		backendv1.ReasonInvalidQuery:           v1alpha.ReasonRefused,
		backendv1.ReasonDestinationNotAllowed:  v1alpha.ReasonRefused,
		backendv1.ReasonServiceDisabled:        v1alpha.ReasonUnavailable,
		backendv1.ReasonLocationNotServed:      v1alpha.ReasonUnavailable,
		backendv1.ReasonRouterUnreachable:      v1alpha.ReasonUnavailable,
		backendv1.ReasonRouterUnavailable:      v1alpha.ReasonUnavailable,
		backendv1.ReasonProbeSourceUnavailable: v1alpha.ReasonUnavailable,
		backendv1.ReasonCancelled:              v1alpha.ReasonUnavailable,
		backendv1.ReasonRateLimited:            v1alpha.ReasonThrottled,
		backendv1.ReasonQueueFull:              v1alpha.ReasonThrottled,
		backendv1.ReasonInventoryTooLarge:      v1alpha.ReasonOutputLimitExceeded,
		backendv1.ReasonResponseTooLarge:       v1alpha.ReasonOutputLimitExceeded,
		backendv1.CompleteOutputLimitExceeded:  v1alpha.ReasonOutputLimitExceeded,
		backendv1.ReasonDeadlineExceeded:       v1alpha.ReasonDeadlineExceeded,
		backendv1.ReasonQueueTimeout:           v1alpha.ReasonDeadlineExceeded,
		backendv1.ReasonDuplicateRequest:       v1alpha.ReasonBackendError,
		backendv1.ReasonParseError:             v1alpha.ReasonBackendError,
		backendv1.ReasonInternal:               v1alpha.ReasonBackendError,
		"SomethingNew":                         v1alpha.ReasonBackendError,
	}
	for backend, reason := range want {
		t.Run(backend, func(t *testing.T) {
			client := &scripted{runErr: backendv1.Refuse(codes.FailedPrecondition, backend, "refused")}
			out := newSink()
			res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
			if res.Reason != reason || res.Coverage != nil {
				t.Fatalf("result = %+v, want %s with no coverage", res, reason)
			}
			frames := out.decoded(t)
			if got := frameTypes(frames); len(got) != 2 || got[0] != FrameHello || got[1] != FrameEnd {
				t.Fatalf("frames = %v, want hello then end", got)
			}
			if _, ok := frames[1]["coverage"]; ok {
				t.Fatal("end frame carries coverage before any snapshot")
			}
		})
	}
	t.Run("transport failure", func(t *testing.T) {
		client := &scripted{runErr: status.Error(codes.Unavailable, "connection refused")}
		res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), newSink())
		if res.Reason != v1alpha.ReasonUnavailable {
			t.Fatalf("reason = %s, want Unavailable", res.Reason)
		}
	})
}

func TestThrottledCarriesRetryAfter(t *testing.T) {
	st, err := status.New(codes.ResourceExhausted, "slow down").WithDetails(
		&errdetails.ErrorInfo{Reason: backendv1.ReasonRateLimited, Domain: backendv1.ErrorDomain},
		&errdetails.RetryInfo{RetryDelay: durationpb.New(1500 * time.Millisecond)},
	)
	if err != nil {
		t.Fatal(err)
	}
	out := newSink()
	res := newAdapter(t, &scripted{runErr: st.Err()}).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
	if res.Reason != v1alpha.ReasonThrottled || res.RetryAfter == nil || *res.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("result = %+v, want Throttled after 1.5s", res)
	}
	if end := out.decoded(t)[1]; end["retryAfter"] != "1.500s" {
		t.Fatalf("end frame retryAfter = %v, want 1.500s", end["retryAfter"])
	}
}

// TestIncompleteStreamsAreNeverComplete feeds streams that are truncated, out
// of order, missing their completion or inconsistent, and expects
// coverage.complete to be false every time.
func TestIncompleteStreamsAreNeverComplete(t *testing.T) {
	ok := completeEvent(&backendv1.Complete{Expected: 2, Successful: 2, Reason: backendv1.CompleteSucceeded})
	cases := map[string]struct {
		events func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent
		end    error
		reason string
	}{
		"truncated after one outcome": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"), observationEvent("router-a", pingObservation()))
			},
			reason: v1alpha.ReasonAgentLost,
		},
		"transport error after the snapshot": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"))
			},
			end:    status.Error(codes.Unavailable, "connection reset"),
			reason: v1alpha.ReasonAgentLost,
		},
		"missing complete": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"),
					observationEvent("router-a", pingObservation()), observationEvent("router-b", pingObservation()))
			},
			reason: v1alpha.ReasonAgentLost,
		},
		"out of order": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				evs := numbered(req, snapshotEvent("router-a", "router-b"),
					observationEvent("router-a", pingObservation()), observationEvent("router-b", pingObservation()), ok)
				evs[1].Sequence, evs[2].Sequence = 3, 2
				return evs
			},
			reason: v1alpha.ReasonBackendError,
		},
		"outcome before the snapshot": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, observationEvent("router-a", pingObservation()), snapshotEvent("router-a", "router-b"))
			},
			reason: v1alpha.ReasonBackendError,
		},
		"duplicate outcome": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"),
					observationEvent("router-a", pingObservation()), observationEvent("router-a", pingObservation()), ok)
			},
			reason: v1alpha.ReasonBackendError,
		},
		"complete with a router unanswered": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"), observationEvent("router-a", pingObservation()), ok)
			},
			reason: v1alpha.ReasonBackendError,
		},
		"complete counts disagree": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				return numbered(req, snapshotEvent("router-a", "router-b"),
					observationEvent("router-a", pingObservation()), observationEvent("router-b", pingObservation()),
					completeEvent(&backendv1.Complete{Expected: 2, Successful: 1, Failed: 1, Reason: backendv1.CompleteSucceeded}))
			},
			reason: v1alpha.ReasonBackendError,
		},
		"payload of another diagnostic": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				wrong := &backendv1.Observation{Result: &backendv1.Observation_BgpSummary{BgpSummary: &backendv1.BGPSummary{}}}
				return numbered(req, snapshotEvent("router-a", "router-b"), observationEvent("router-a", wrong))
			},
			reason: v1alpha.ReasonBackendError,
		},
		"omitted router without a contract reason": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				snap := snapshotEvent("router-a")
				snap.GetSnapshot().Routers = append(snap.GetSnapshot().Routers, &backendv1.SnapshotRouter{Router: "router-b"})
				return numbered(req, snap)
			},
			reason: v1alpha.ReasonBackendError,
		},
		"event for another request": {
			events: func(req *backendv1.RunDiagnosticRequest) []*backendv1.DiagnosticEvent {
				evs := numbered(req, snapshotEvent("router-a", "router-b"), observationEvent("router-a", pingObservation()))
				evs[1].RequestId = uuid.NewString()
				return evs
			},
			reason: v1alpha.ReasonBackendError,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := &scripted{script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
				return tc.events(req), tc.end
			}}
			out := newSink()
			res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
			if res.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s", res.Reason, tc.reason)
			}
			if res.Coverage != nil && res.Coverage.Complete {
				t.Fatalf("coverage = %+v, want incomplete", res.Coverage)
			}
			frames := out.decoded(t)
			end := frames[len(frames)-1]
			if end["type"] != FrameEnd {
				t.Fatalf("last frame = %v, want end", end["type"])
			}
			if cov, ok := end["coverage"].(map[string]any); ok && cov["complete"] != false {
				t.Fatalf("end coverage = %v, want complete false", cov)
			}
		})
	}
}

// TestBackendCancelledWithoutACauseIsUnavailable covers a Complete reason of
// Cancelled when the gateway stopped nothing: the backend stopped on its own.
func TestBackendCancelledWithoutACauseIsUnavailable(t *testing.T) {
	client := &scripted{script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
		return numbered(req, snapshotEvent("router-a"), failureEvent("router-a", backendv1.ReasonCancelled),
			completeEvent(&backendv1.Complete{Expected: 1, Failed: 1, Reason: backendv1.CompleteCancelled})), nil
	}}
	res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), newSink())
	if res.Reason != v1alpha.ReasonUnavailable || !res.Coverage.Complete {
		t.Fatalf("result = %+v, want Unavailable with complete coverage", res)
	}
}

// TestCancellationStopsTheBackendCall cancels a running session for each
// cause the gateway knows and expects the backend call to stop, exactly one
// call, and the matching terminal reason.
func TestCancellationStopsTheBackendCall(t *testing.T) {
	cases := map[string]struct {
		cause   error
		reason  string
		endSent bool
	}{
		"client disconnect": {ErrDisconnected, v1alpha.ReasonDisconnected, false},
		"client close":      {ErrClosedByUser, v1alpha.ReasonClosedByUser, false},
		"revocation":        {ErrRevoked, v1alpha.ReasonRevoked, true},
		"gateway shutdown":  {ErrShutdown, v1alpha.ReasonAgentLost, true},
		"expiry":            {nil, v1alpha.ReasonDeadlineExceeded, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := fakeOptions()
			opts.OutcomeDelay = time.Minute
			b := startBackend(t, opts)
			s := session(pingDiagnostic("198.51.100.7"))
			if tc.cause == nil {
				// The deadline passes two seconds after the claim.
				s.StartedAt = time.Now().Add(-ExecutionTimeout + 2*time.Second)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			out := newSink()
			done := make(chan Result, 1)
			go func() { done <- newAdapter(t, b.client).Run(ctx, s, out) }()

			out.waitFor(t, FrameSnapshot)
			if tc.cause != nil {
				cancel(tc.cause)
			}
			var res Result
			select {
			case res = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not return within 3s")
			}
			b.waitEnded(t, 2*time.Second)
			if n := b.runCalls.Load(); n != 1 {
				t.Fatalf("backend calls = %d, want 1", n)
			}
			if res.Reason != tc.reason || res.EndSent != tc.endSent {
				t.Fatalf("result = %+v, want %s with endSent %v", res, tc.reason, tc.endSent)
			}
			// At expiry the fake may finish its own complete event first, so
			// only a cause the gateway sets must leave coverage incomplete.
			if res.Coverage == nil || (tc.cause != nil && res.Coverage.Complete) {
				t.Fatalf("coverage = %+v, want incomplete", res.Coverage)
			}
		})
	}
}

// TestSeqStaysConsecutiveWhenAFrameIsDropped revokes a session while the
// gateway waits for buffer room, so the waiting frame is dropped. The end
// frame must still follow the last delivered frame's seq.
func TestSeqStaysConsecutiveWhenAFrameIsDropped(t *testing.T) {
	opts := fakeOptions()
	opts.Routers = []string{"r1", "r2", "r3", "r4"}
	b := startBackend(t, opts)
	a := newAdapter(t, b.client, func(c *Config) {
		c.BufferedFrames = 1
		c.SlowClientTimeout = time.Minute
	})
	out := &gatedSink{release: make(chan struct{}), inner: newSink()}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan Result, 1)
	go func() { done <- a.Run(ctx, session(pingDiagnostic("198.51.100.7")), out) }()

	// The sink holds hello, the buffer holds the snapshot, and the gateway
	// waits to queue the first outcome.
	deadline := time.Now().Add(5 * time.Second)
	for b.received.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("gateway never read two backend events")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel(ErrRevoked)
	close(out.release)
	res := <-done
	if res.Reason != v1alpha.ReasonRevoked || !res.EndSent {
		t.Fatalf("result = %+v, want Revoked with the end frame sent", res)
	}
	frames := out.inner.decoded(t)
	for i, f := range frames {
		if seq := f["seq"].(float64); int(seq) != i+1 {
			t.Fatalf("frame %d (%s) has seq %v; frames %v", i, f["type"], seq, frameTypes(frames))
		}
	}
	if last := frames[len(frames)-1]; last["type"] != FrameEnd {
		t.Fatalf("last frame = %v, want end", last["type"])
	}
}

// TestKnownCauseOutranksTheBackendReason revokes the session as the backend's
// complete event arrives. Revoked ranks above the backend's own reason.
func TestKnownCauseOutranksTheBackendReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &scripted{
		script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
			return numbered(req, snapshotEvent("router-a"), observationEvent("router-a", pingObservation()),
				completeEvent(&backendv1.Complete{Expected: 1, Successful: 1, Reason: backendv1.CompleteSucceeded})), nil
		},
		beforeComplete: func() { cancel(ErrRevoked) },
	}
	res := newAdapter(t, client).Run(ctx, session(pingDiagnostic("198.51.100.7")), newSink())
	if res.Reason != v1alpha.ReasonRevoked {
		t.Fatalf("reason = %s, want Revoked", res.Reason)
	}
}

// TestOversizedEventIsAnOutputLimit covers an event over MaxEventBytes after
// the snapshot, which gRPC reports as ResourceExhausted.
func TestOversizedEventIsAnOutputLimit(t *testing.T) {
	client := &scripted{script: func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error) {
		return numbered(req, snapshotEvent("router-a")), status.Error(codes.ResourceExhausted, "message larger than max")
	}}
	res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), newSink())
	if res.Reason != v1alpha.ReasonOutputLimitExceeded || res.Coverage.Complete {
		t.Fatalf("result = %+v, want OutputLimitExceeded with incomplete coverage", res)
	}
}

// TestShutdownBeforeTheSnapshotIsUnavailable stops the gateway while the
// capabilities check is the only thing that has happened.
func TestShutdownBeforeTheSnapshotIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(ErrShutdown)
	client := &scripted{capsErr: status.Error(codes.Canceled, "context canceled")}
	res := newAdapter(t, client).Run(ctx, session(pingDiagnostic("198.51.100.7")), newSink())
	if res.Reason != v1alpha.ReasonUnavailable || res.Coverage != nil {
		t.Fatalf("result = %+v, want Unavailable with no coverage", res)
	}
	if n := client.runCalls.Load(); n != 0 {
		t.Fatalf("backend calls = %d, want 0", n)
	}
}

// TestSlowClientIsDisconnected blocks the client and expects the gateway to
// stop reading from the backend at the buffer bound, cut the client off and
// cancel the backend call.
func TestSlowClientIsDisconnected(t *testing.T) {
	opts := fakeOptions()
	opts.Routers = []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8"}
	b := startBackend(t, opts)
	const buffered = 2
	a := newAdapter(t, b.client, func(c *Config) {
		c.BufferedFrames = buffered
		c.SlowClientTimeout = 200 * time.Millisecond
	})
	out := &blockingSink{}
	start := time.Now()
	res := a.Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Run took %s", elapsed)
	}
	if res.Reason != v1alpha.ReasonDisconnected || res.EndSent {
		t.Fatalf("result = %+v, want Disconnected with no end frame", res)
	}
	b.waitEnded(t, 2*time.Second)
	// The sink holds one frame and the buffer the rest; the gateway holds at
	// most one more event while it waits for room. The backend has ten.
	if got := b.received.Load(); got > buffered+1 {
		t.Fatalf("gateway read %d backend events with a %d-frame buffer", got, buffered)
	}
	if n := b.runCalls.Load(); n != 1 {
		t.Fatalf("backend calls = %d, want 1", n)
	}
}

// TestUnsupportedAPIMajorRefusesBeforeAnyCall covers a backend that reports
// another API major, and one that refuses ours.
func TestUnsupportedAPIMajorRefusesBeforeAnyCall(t *testing.T) {
	cases := map[string]*scripted{
		"backend reports major 2": {apiMajor: 2},
		"backend refuses major 1": {capsErr: backendv1.Refuse(codes.FailedPrecondition, backendv1.ReasonVersionUnsupported, "unsupported")},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			out := newSink()
			res := newAdapter(t, client).Run(t.Context(), session(pingDiagnostic("198.51.100.7")), out)
			if res.Reason != v1alpha.ReasonRefused {
				t.Fatalf("reason = %s, want Refused", res.Reason)
			}
			if n := client.runCalls.Load(); n != 0 {
				t.Fatalf("RunDiagnostic calls = %d, want 0", n)
			}
			if got := frameTypes(out.decoded(t)); len(got) != 2 || got[1] != FrameEnd {
				t.Fatalf("frames = %v, want hello then end", got)
			}
		})
	}
}

func TestCapabilitiesAreReused(t *testing.T) {
	b := startBackend(t, fakeOptions())
	a := newAdapter(t, b.client)
	for range 2 {
		if res := a.Run(t.Context(), session(diagnostics["bgp-summary-v4"]), newSink()); res.Reason != v1alpha.ReasonSucceeded {
			t.Fatalf("reason = %s", res.Reason)
		}
	}
	if n := b.capsCalls.Load(); n != 1 {
		t.Fatalf("GetCapabilities calls = %d, want 1", n)
	}
}

func TestInvalidSessionNeverCallsTheBackend(t *testing.T) {
	client := &scripted{}
	s := session(pingDiagnostic("198.51.100.7"))
	s.ProjectID = ""
	res := newAdapter(t, client).Run(t.Context(), s, newSink())
	if res.Reason != v1alpha.ReasonBackendError || client.runCalls.Load() != 0 {
		t.Fatalf("result = %+v after %d calls, want BackendError and no call", res, client.runCalls.Load())
	}
}

func TestQueryCanonicalizesTheTarget(t *testing.T) {
	cases := []struct {
		d      v1alpha.Diagnostic
		family backendv1.AddressFamily
		target string
		err    bool
	}{
		{d: diagnostics["route-lookup-v4"], family: backendv1.AddressFamily_ADDRESS_FAMILY_IPV4, target: "198.51.100.7"},
		{d: diagnostics["route-lookup-v6"], family: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6, target: "2001:db8::/32"},
		{d: diagnostics["bgp-summary-v6"], family: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6},
		{d: diagnostics["traceroute-v6"], family: backendv1.AddressFamily_ADDRESS_FAMILY_IPV6, target: "2001:db8::7"},
		{d: v1alpha.Diagnostic{Type: v1alpha.DiagnosticRouteLookup, RouteLookup: &v1alpha.RouteLookupDiagnostic{Destination: "198.51.100.1/24"}}, err: true},
		{d: v1alpha.Diagnostic{Type: v1alpha.DiagnosticRouteLookup, RouteLookup: &v1alpha.RouteLookupDiagnostic{Destination: "198.51.100.0/24", AddressFamily: v1alpha.AddressFamilyIPv6}}, err: true},
		{d: pingDiagnostic("::ffff:198.51.100.7"), err: true},
		{d: v1alpha.Diagnostic{Type: v1alpha.DiagnosticPing}, err: true},
	}
	for _, tc := range cases {
		q, err := Query(tc.d)
		if tc.err {
			if err == nil {
				t.Errorf("Query(%+v) succeeded, want an error", tc.d)
			}
			continue
		}
		if err != nil || q.GetAddressFamily() != tc.family || q.GetTarget() != tc.target {
			t.Errorf("Query(%+v) = %v, %v; want %s %q", tc.d, q, err, tc.family, tc.target)
		}
	}
}

func TestFailureMessageIsBounded(t *testing.T) {
	f := failure(&backendv1.Failure{Reason: backendv1.ReasonInternal, Message: strings.Repeat("é", 200)})
	if len(f.Message) > maxFailureMessage || !strings.HasPrefix(strings.Repeat("é", 200), f.Message) {
		t.Fatalf("message is %d bytes or split a character", len(f.Message))
	}
}

func TestCauseReasonPrefersKnownCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("something else"))
	if got := causeReason(ctx, true); got != "" {
		t.Fatalf("unknown cause gave %q", got)
	}
}
