package routerbackend

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/pkg/driver"
)

// run is one admitted diagnostic.
type run struct {
	s      *Server
	req    *backendv1.RunDiagnosticRequest
	q      *query
	loc    *location
	stream grpc.ServerStreamingServer[backendv1.DiagnosticEvent]
	seq    uint64
}

// outcome is one selected router's result.
type outcome struct {
	router    *router
	event     *backendv1.RouterOutcome
	truncated bool
}

// execute sends the snapshot, fans out to the selected routers and sends each
// outcome as it arrives, then the complete event. It returns the complete
// event's reason, or "" when the caller went away first.
func (r *run) execute(ctx context.Context) (string, error) {
	snap, selected, ineligible := r.snapshot()
	if err := r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Snapshot{Snapshot: snap}}); err != nil {
		return "", err
	}

	// Buffered so that a router finishing after the caller left never blocks.
	results := make(chan outcome, len(selected))
	for _, rt := range selected {
		go func() { results <- r.runRouter(ctx, rt) }()
	}

	complete := &backendv1.Complete{
		SnapshotId: snap.GetSnapshotId(),
		Expected:   uint32(len(snap.GetRouters())),
		Omitted:    uint32(len(snap.GetRouters()) - len(selected)),
	}
	for range selected {
		var o outcome
		select {
		case o = <-results:
		case <-r.stream.Context().Done():
			// The caller is gone. Every router's context is cancelled, and
			// nothing more can be delivered.
			return "", status.Error(codes.Canceled, "caller cancelled the request")
		}
		reason := "Succeeded"
		if f := o.event.GetFailure(); f != nil {
			complete.Failed++
			reason = f.GetReason()
		} else {
			complete.Successful++
			if o.truncated {
				complete.Truncated++
			}
		}
		routerOutcomesTotal.WithLabelValues(r.loc.name, o.router.name, string(r.q.diag), reason).Inc()
		if err := r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: o.event}}); err != nil {
			return "", err
		}
	}
	complete.FinishedAt = timestamppb.New(r.s.now())
	complete.Reason = terminalReason(ctx, ineligible, complete)
	return complete.Reason, r.send(&backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Complete{Complete: complete}})
}

// snapshot fixes the router set: routers whose driver serves the query are
// selected in inventory order up to max routers, the rest are omitted as
// NotRequested; routers that don't serve it are omitted as Ineligible.
func (r *run) snapshot() (*backendv1.Snapshot, []*router, bool) {
	snap := &backendv1.Snapshot{SnapshotId: uuid.NewString(), CapturedAt: timestamppb.New(r.s.now())}
	var selected []*router
	ineligible := false
	for _, rt := range r.loc.routers {
		sr := &backendv1.SnapshotRouter{Router: rt.name}
		switch {
		case !rt.drv.Capabilities().Serves(r.q.diag, r.q.family):
			sr.OmittedReason = backendv1.OmittedIneligible
			ineligible = true
		case len(selected) >= r.s.cfg.Limits.MaxRouters:
			sr.OmittedReason = backendv1.OmittedNotRequested
		default:
			sr.Selected = true
			selected = append(selected, rt)
		}
		snap.Routers = append(snap.Routers, sr)
	}
	return snap, selected, ineligible
}

// runRouter waits for a slot on the router, then runs the diagnostic with
// the router's own timeout. It returns by the time ctx or the router's
// timeout ends, even if the driver has not returned yet: the driver call
// then keeps the router's slot until it does.
func (r *run) runRouter(ctx context.Context, rt *router) outcome {
	release, reason := rt.slots.acquire(ctx)
	if reason != "" {
		f := &backendv1.Failure{Reason: reason, Message: "the router could not take the diagnostic in time"}
		if reason == backendv1.ReasonQueueFull || reason == backendv1.ReasonQueueTimeout {
			f.Scope = scopeRouter
			f.RetryAfter = durationpb.New(r.s.cfg.Budgets.RouterQueueWait.Duration)
		}
		return r.failure(rt, f)
	}

	rctx, cancel := context.WithTimeout(ctx, r.s.cfg.Limits.RouterTimeout.Duration)
	type result struct {
		value any
		err   error
	}
	done := make(chan result, 1)
	start := r.s.now()
	go func() {
		defer release()
		defer cancel()
		v, err := r.call(rctx, rt.drv)
		routerSeconds.WithLabelValues(string(r.q.diag)).Observe(time.Since(start).Seconds())
		done <- result{v, err}
	}()

	var res result
	select {
	case res = <-done:
	case <-rctx.Done():
		res.err = rctx.Err()
	}
	if res.err == nil {
		obs, truncated, err := r.observe(rt, res.value, start)
		if err == nil {
			return outcome{router: rt, truncated: truncated, event: &backendv1.RouterOutcome{
				Router: rt.name, CompletedAt: timestamppb.New(r.s.now()),
				Result: &backendv1.RouterOutcome_Observation{Observation: obs},
			}}
		}
		res.err = err
	}
	return r.failure(rt, r.classify(ctx, res.err))
}

// call runs the query on one driver with the contract's fixed parameters.
func (r *run) call(ctx context.Context, d driver.Driver) (any, error) {
	switch r.q.diag {
	case driver.DiagnosticRouteLookup:
		return d.RouteLookup(ctx, driver.RouteLookupRequest{Target: r.q.prefix, MaxPaths: MaxPathsPerPrefix})
	case driver.DiagnosticBGPSummary:
		return d.BGPSummary(ctx, driver.BGPSummaryRequest{Family: r.q.family})
	case driver.DiagnosticPing:
		return d.Ping(ctx, driver.PingRequest{Destination: r.q.addr, Count: PingCount, Wait: ProbeWait, PayloadBytes: ProbePayloadBytes})
	default:
		return d.Traceroute(ctx, driver.TracerouteRequest{
			Destination: r.q.addr, MaxHops: TracerouteMaxHops, ProbesPerHop: TracerouteProbes,
			Wait: ProbeWait, PayloadBytes: ProbePayloadBytes,
		})
	}
}

// errNoResult reports a driver that returned neither a result nor an error.
var errNoResult = driver.Errorf(driver.ReasonInternal, "the driver returned no result")

// observe converts a driver result into a redacted observation.
func (r *run) observe(rt *router, value any, start time.Time) (*backendv1.Observation, bool, error) {
	obs := &backendv1.Observation{
		Asn:        rt.asn,
		SampleTime: timestamppb.New(start),
		Duration:   durationpb.New(r.s.now().Sub(start)),
	}
	switch v := value.(type) {
	case *driver.RouteResult:
		if v == nil {
			return nil, false, errNoResult
		}
		routes, truncated := r.s.redact.routes(v, rt.asn)
		obs.Truncated = truncated
		obs.Result = &backendv1.Observation_Routes{Routes: routes}
	case *driver.BGPSummary:
		if v == nil {
			return nil, false, errNoResult
		}
		obs.Result = &backendv1.Observation_BgpSummary{BgpSummary: bgpSummary(v)}
	case *driver.PingResult:
		if v == nil {
			return nil, false, errNoResult
		}
		obs.Result = &backendv1.Observation_Ping{Ping: r.s.redact.ping(v)}
	case *driver.TracerouteResult:
		if v == nil {
			return nil, false, errNoResult
		}
		obs.Result = &backendv1.Observation_Traceroute{Traceroute: r.s.redact.traceroute(v)}
	default:
		return nil, false, errNoResult
	}
	return obs, obs.GetTruncated(), nil
}

// classify turns a driver error into a typed failure. The diagnostic's own
// end wins: past expires_at every router reports DeadlineExceeded, and after
// a cancellation Cancelled.
func (r *run) classify(ctx context.Context, err error) *backendv1.Failure {
	if ctx.Err() != nil {
		return &backendv1.Failure{Reason: stopReason(ctx), Message: "the diagnostic ended before the router finished"}
	}
	reason := driver.ReasonOf(err)
	if !knownReasons[reason] {
		reason = driver.ReasonInternal
	}
	msg := "the router failed"
	var de *driver.Error
	if errors.As(err, &de) && de.Message != "" {
		msg = de.Message
	} else if reason == driver.ReasonTimeout {
		msg = "the router did not finish in time"
	}
	r.s.log.Info("router failed", "requestID", r.req.GetRequestId(), "location", r.loc.name, "reason", string(reason), "error", err)
	return &backendv1.Failure{Reason: string(reason), Message: truncate(msg, maxFailureMessage)}
}

// knownReasons are the driver reasons the contract maps; any other is
// reported as Internal.
var knownReasons = map[driver.Reason]bool{
	driver.ReasonRouterUnreachable: true, driver.ReasonRouterUnavailable: true, driver.ReasonRouterBusy: true,
	driver.ReasonTimeout: true, driver.ReasonCancelled: true, driver.ReasonQueryTypeUnavailable: true,
	driver.ReasonProbeSourceUnavailable: true, driver.ReasonDestinationNotAllowed: true,
	driver.ReasonResponseTooLarge: true, driver.ReasonParseError: true, driver.ReasonInternal: true,
}

func (r *run) failure(rt *router, f *backendv1.Failure) outcome {
	return outcome{router: rt, event: &backendv1.RouterOutcome{
		Router: rt.name, CompletedAt: timestamppb.New(r.s.now()),
		Result: &backendv1.RouterOutcome_Failure{Failure: f},
	}}
}

func (r *run) send(ev *backendv1.DiagnosticEvent) error {
	r.seq++
	ev.RequestId = r.req.GetRequestId()
	ev.Sequence = r.seq
	return r.stream.Send(ev)
}

// terminalReason follows the contract's precedence: the deadline wins, then
// coverage decides. An ineligible router makes a result partial; a router the
// request did not ask for does not.
func terminalReason(ctx context.Context, ineligible bool, c *backendv1.Complete) string {
	switch {
	case ctx.Err() != nil && stopReason(ctx) == backendv1.ReasonDeadlineExceeded:
		return backendv1.CompleteDeadlineExceeded
	case c.GetSuccessful()+c.GetFailed() == 0:
		return backendv1.CompleteNoNodesAvailable
	case c.GetSuccessful() == 0:
		return backendv1.CompleteExecutionFailed
	case c.GetFailed() > 0 || c.GetTruncated() > 0 || ineligible:
		return backendv1.CompletePartialResults
	default:
		return backendv1.CompleteSucceeded
	}
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
