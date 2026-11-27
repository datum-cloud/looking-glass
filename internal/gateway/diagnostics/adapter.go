// Package diagnostics is the gateway's backend adapter. It turns one claimed
// session into exactly one RunDiagnostic call to the cell's backend, and
// streams the call's events to the client as the frames
// docs/api/v1alpha-contract.md ("Connection and stream", "Backend mapping")
// defines.
//
// The gateway owns the client connection and the single-use claim. It calls
// Run once per claimed session with a Sink that writes WebSocket text
// messages, and cancels Run's context with one of the Err* causes when the
// client disconnects or closes, the session is revoked, or the gateway shuts
// down. Run applies the 90 s execution deadline itself.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"google.golang.org/grpc"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

// Defaults for Config's zero values.
const (
	DefaultBufferedFrames    = 32
	DefaultSlowClientTimeout = 5 * time.Second
	DefaultFlushTimeout      = 2 * time.Second
	DefaultMaxEventBytes     = 1 << 20
	DefaultCapabilitiesTTL   = time.Minute
)

// Sink delivers frames to the client. Each frame is one JSON object, sent as
// one WebSocket text message.
type Sink interface {
	// WriteFrame writes one frame. It may block while the client is slow, and
	// must return once ctx ends.
	WriteFrame(ctx context.Context, frame []byte) error
}

// Config configures an Adapter.
type Config struct {
	// Client calls the cell's backend. Dial its connection with DialOptions,
	// so that gRPC never retries a call.
	Client backendv1.DiagnosticServiceClient
	// BufferedFrames bounds the frames waiting for a slow client. With
	// MaxEventBytes it bounds the memory one session holds.
	BufferedFrames int
	// SlowClientTimeout is how long Run waits for room in a full buffer before
	// it disconnects the client.
	SlowClientTimeout time.Duration
	// FlushTimeout bounds the delivery of the end frame and the frames before
	// it once the session has ended.
	FlushTimeout time.Duration
	// MaxEventBytes bounds one backend event. A larger event fails the call.
	MaxEventBytes int
	// CapabilitiesTTL is how long a successful capabilities check is reused.
	CapabilitiesTTL time.Duration
	// Log receives one line per session. It never holds keys, signatures or
	// router names.
	Log logr.Logger
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// DialOptions are the options the gateway's backend connection needs:
// application- and transport-level retries are off, so an interrupted
// diagnostic is never replayed.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithDisableRetry()}
}

// Adapter runs claimed sessions against one backend.
type Adapter struct {
	cfg Config

	mu sync.Mutex
	// capsValid holds, per cell, when the last successful capabilities check
	// stops being reused.
	capsValid map[string]time.Time
}

// New returns an Adapter, or an error when cfg has no client.
func New(cfg Config) (*Adapter, error) {
	if cfg.Client == nil {
		return nil, errors.New("a backend client is required")
	}
	if cfg.BufferedFrames <= 0 {
		cfg.BufferedFrames = DefaultBufferedFrames
	}
	if cfg.SlowClientTimeout <= 0 {
		cfg.SlowClientTimeout = DefaultSlowClientTimeout
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = DefaultFlushTimeout
	}
	if cfg.MaxEventBytes <= 0 {
		cfg.MaxEventBytes = DefaultMaxEventBytes
	}
	if cfg.CapabilitiesTTL <= 0 {
		cfg.CapabilitiesTTL = DefaultCapabilitiesTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Adapter{cfg: cfg, capsValid: map[string]time.Time{}}, nil
}

// Result is how a session ended. The gateway writes it to the session's
// status.
type Result struct {
	// Reason is the session's terminal reason.
	Reason string
	// Message is the end frame's message.
	Message string
	// Coverage is nil when the session ended before the backend's snapshot.
	Coverage *v1alpha.Coverage
	// RetryAfter is the backend's advice, for Throttled only.
	RetryAfter *time.Duration
	// FinishedAt is when the session ended.
	FinishedAt time.Time
	// EndSent reports that the client received the end frame. The gateway
	// then closes the connection with code 1000.
	EndSent bool
}

// Run runs session s, writing its frames to sink, and returns how it ended.
// It makes at most one RunDiagnostic call and never retries it. It returns
// once the backend call has stopped and sink is no longer written to.
func (a *Adapter) Run(ctx context.Context, s Session, sink Sink) Result {
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	execCtx, cancel := context.WithDeadlineCause(runCtx, s.Deadline(), errDeadline)
	defer cancel()

	r := &run{
		a:   a,
		s:   s,
		ctx: execCtx,
		out: newOutbox(context.WithoutCancel(ctx), sink, a.cfg, stop),
	}
	reason, retry := r.execute()
	res := Result{
		Reason:     reason,
		Message:    endMessages[reason],
		RetryAfter: retry,
		FinishedAt: a.cfg.Now().UTC(),
	}
	if r.tracker != nil {
		res.Coverage = r.tracker.coverage()
	}

	var end []byte
	// A client that disconnected or closed the stream receives nothing more.
	if reason != v1alpha.ReasonDisconnected && reason != v1alpha.ReasonClosedByUser {
		frame := End{
			Type:       FrameEnd,
			Seq:        r.nextSeq(),
			Reason:     res.Reason,
			Message:    res.Message,
			Coverage:   res.Coverage,
			FinishedAt: res.FinishedAt,
		}
		if retry != nil {
			d := Duration(*retry)
			frame.RetryAfter = &d
		}
		end, _ = json.Marshal(frame)
	}
	res.EndSent = r.out.finish(end, a.cfg.FlushTimeout)

	sessionsTotal.WithLabelValues(diagnosticLabel(s.Diagnostic.Type), reason).Inc()
	a.cfg.Log.Info("session ended", "session", s.UID, "location", s.Location,
		"diagnostic", string(s.Diagnostic.Type), "reason", reason, "endSent", res.EndSent)
	return res
}

// run is one session's execution.
type run struct {
	a       *Adapter
	s       Session
	ctx     context.Context
	out     *outbox
	seq     uint64
	tracker *tracker
}

func (r *run) nextSeq() uint64 {
	r.seq++
	return r.seq
}

// send queues frame for the client, numbering it.
func (r *run) send(frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return r.out.push(r.ctx, b)
}

// execute runs the session up to its terminal reason.
func (r *run) execute() (string, *time.Duration) {
	hello := Hello{
		Type:       FrameHello,
		Seq:        r.nextSeq(),
		Session:    r.s.UID,
		Location:   r.s.Location,
		Cell:       r.s.Cell,
		Diagnostic: r.s.Diagnostic,
		Deadline:   r.s.Deadline().UTC(),
	}
	if err := r.send(hello); err != nil {
		return r.stopped(), nil
	}
	req, err := r.s.request(uuid.NewString())
	if err != nil {
		r.a.cfg.Log.Error(err, "cannot build the backend request", "session", r.s.UID)
		return v1alpha.ReasonBackendError, nil
	}
	if reason, retry := r.a.checkCapabilities(r.ctx, r.s.Cell); reason != "" {
		return reason, retry
	}
	return r.stream(req)
}

// stopped is the terminal reason once the session was stopped from outside
// the backend stream.
func (r *run) stopped() string {
	snapshot := r.tracker != nil && r.tracker.snapshot != nil
	if reason := causeReason(r.ctx, snapshot); reason != "" {
		return reason
	}
	if snapshot {
		return v1alpha.ReasonAgentLost
	}
	return v1alpha.ReasonUnavailable
}

// stream makes the one RunDiagnostic call and relays its events.
func (r *run) stream(req *backendv1.RunDiagnosticRequest) (string, *time.Duration) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	start := time.Now()
	defer func() {
		backendCallSeconds.WithLabelValues(diagnosticLabel(r.s.Diagnostic.Type)).Observe(time.Since(start).Seconds())
	}()

	st, err := r.a.cfg.Client.RunDiagnostic(ctx, req, grpc.MaxCallRecvMsgSize(r.a.cfg.MaxEventBytes))
	if err != nil {
		return callErrorReason(r.ctx, err)
	}
	r.tracker = newTracker(req)
	for {
		ev, err := st.Recv()
		if err != nil {
			if r.tracker.snapshot == nil {
				return callErrorReason(r.ctx, err)
			}
			// The stream ended after the snapshot without a complete event.
			return r.stopped(), nil
		}
		if err := r.tracker.accept(ev); err != nil {
			return r.violation(err), nil
		}
		frame, err := r.frame(req.GetQuery().GetType(), ev)
		if err != nil {
			return r.violation(err), nil
		}
		if frame != nil {
			if err := r.send(frame); err != nil {
				return r.stopped(), nil
			}
		}
		if c := r.tracker.complete; c != nil {
			if reason := completeReason(c.GetReason()); reason != "" {
				return reason, nil
			}
			// The backend cancelled the work. A cause the gateway knows
			// explains why; otherwise the backend stopped on its own.
			if reason := causeReason(r.ctx, true); reason != "" {
				return reason, nil
			}
			return v1alpha.ReasonUnavailable, nil
		}
	}
}

func (r *run) violation(err error) string {
	streamViolationsTotal.Inc()
	r.a.cfg.Log.Error(err, "backend stream rejected", "session", r.s.UID)
	return v1alpha.ReasonBackendError
}

// frame converts a backend event to the client frame it becomes. A complete
// event becomes no frame: the end frame follows once Run has its result.
func (r *run) frame(t backendv1.DiagnosticType, ev *backendv1.DiagnosticEvent) (any, error) {
	switch e := ev.GetEvent().(type) {
	case *backendv1.DiagnosticEvent_Snapshot:
		frame := Snapshot{Type: FrameSnapshot, Routers: []SnapshotRouter{}}
		for _, router := range e.Snapshot.GetRouters() {
			sr := SnapshotRouter{Router: RouterID(r.s.Location, router.GetRouter()), Selected: router.GetSelected()}
			if !sr.Selected {
				sr.OmittedReason = router.GetOmittedReason()
			}
			frame.Routers = append(frame.Routers, sr)
		}
		frame.Seq = r.nextSeq()
		return frame, nil
	case *backendv1.DiagnosticEvent_Outcome:
		o := e.Outcome
		frame := Outcome{
			Type:        FrameOutcome,
			Router:      RouterID(r.s.Location, o.GetRouter()),
			CompletedAt: timestamp(o.GetCompletedAt()),
		}
		label := v1alpha.ReasonSucceeded
		if o.GetFailure() != nil {
			frame.Failure = failure(o.GetFailure())
			label = frame.Failure.Reason
		} else {
			obs, err := observation(t, o.GetObservation())
			if err != nil {
				return nil, violation("%s", err)
			}
			frame.Observation = obs
		}
		routerOutcomesTotal.WithLabelValues(diagnosticLabel(r.s.Diagnostic.Type), label).Inc()
		frame.Seq = r.nextSeq()
		return frame, nil
	default:
		return nil, nil
	}
}

// checkCapabilities refuses the session when the backend does not serve this
// API major. It returns "" when the session may run.
func (a *Adapter) checkCapabilities(ctx context.Context, cell string) (string, *time.Duration) {
	a.mu.Lock()
	valid := a.cfg.Now().Before(a.capsValid[cell])
	a.mu.Unlock()
	if valid {
		return "", nil
	}
	resp, err := a.cfg.Client.GetCapabilities(ctx, &backendv1.GetCapabilitiesRequest{ApiMajor: backendv1.APIMajor, Cell: cell})
	if err != nil {
		return callErrorReason(ctx, err)
	}
	if resp.GetApiMajor() != backendv1.APIMajor {
		a.cfg.Log.Info("backend API major not supported", "cell", cell, "apiMajor", resp.GetApiMajor())
		return v1alpha.ReasonRefused, nil
	}
	a.mu.Lock()
	a.capsValid[cell] = a.cfg.Now().Add(a.cfg.CapabilitiesTTL)
	a.mu.Unlock()
	return "", nil
}

// errClientGone means the client can no longer receive frames.
var errClientGone = errors.New("client can no longer receive frames")

// outbox holds the frames waiting for the client, at most a fixed number,
// and writes them to the sink in order from one goroutine.
type outbox struct {
	sink   Sink
	frames chan []byte
	slow   time.Duration
	stop   context.CancelCauseFunc

	// wctx bounds sink writes. It outlives the session's context so that the
	// end frame can still be written after a revocation or a shutdown.
	wctx    context.Context
	wcancel context.CancelFunc

	gone     chan struct{}
	goneOnce sync.Once
	done     chan struct{}
}

func newOutbox(parent context.Context, sink Sink, cfg Config, stop context.CancelCauseFunc) *outbox {
	wctx, wcancel := context.WithCancel(parent)
	o := &outbox{
		sink:    sink,
		frames:  make(chan []byte, cfg.BufferedFrames),
		slow:    cfg.SlowClientTimeout,
		stop:    stop,
		wctx:    wctx,
		wcancel: wcancel,
		gone:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go o.loop()
	return o
}

func (o *outbox) loop() {
	defer close(o.done)
	for f := range o.frames {
		if err := o.sink.WriteFrame(o.wctx, f); err != nil {
			o.markGone(ErrDisconnected)
			for range o.frames {
				// Discard the rest until finish closes the channel.
			}
			return
		}
	}
}

// markGone records that the client can no longer receive, unblocks any write
// in progress and stops the session with cause.
func (o *outbox) markGone(cause error) {
	o.goneOnce.Do(func() {
		close(o.gone)
		o.wcancel()
		o.stop(cause)
	})
}

// push queues frame. When the buffer stays full for the slow-client timeout,
// it disconnects the client and returns ErrSlowClient.
func (o *outbox) push(ctx context.Context, frame []byte) error {
	select {
	case <-o.gone:
		return errClientGone
	case o.frames <- frame:
		return nil
	default:
	}
	t := time.NewTimer(o.slow)
	defer t.Stop()
	select {
	case o.frames <- frame:
		return nil
	case <-o.gone:
		return errClientGone
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		slowClientsTotal.Inc()
		o.markGone(ErrSlowClient)
		return ErrSlowClient
	}
}

// finish queues end, if any, waits up to flush for the writer to drain, and
// reports whether the client received end.
func (o *outbox) finish(end []byte, flush time.Duration) bool {
	deadline, cancel := context.WithTimeout(context.Background(), flush)
	defer cancel()
	queued := false
	if end != nil {
		select {
		case o.frames <- end:
			queued = true
		case <-o.gone:
		case <-deadline.Done():
		}
	}
	close(o.frames)
	select {
	case <-o.done:
	case <-deadline.Done():
		o.markGone(ErrSlowClient)
		<-o.done
	}
	select {
	case <-o.gone:
		return false
	default:
		o.wcancel()
		return queued
	}
}
