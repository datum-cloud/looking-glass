package diagnostics

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/fakebackend"
)

// testProject is a synthetic project UID.
const testProject = "project-uid-0001"

// backend is the fake backend served over an in-memory gRPC connection,
// with a record of the calls it received.
type backend struct {
	client backendv1.DiagnosticServiceClient

	runCalls  atomic.Int32
	capsCalls atomic.Int32
	// received counts the events the gateway's client read.
	received atomic.Int32

	mu sync.Mutex
	// requests holds every RunDiagnostic request the fake received.
	requests []*backendv1.RunDiagnosticRequest
	// ended is closed when a RunDiagnostic handler returns.
	ended   chan struct{}
	endOnce sync.Once
}

func startBackend(t *testing.T, opts fakebackend.Options) *backend {
	t.Helper()
	fake, err := fakebackend.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{ended: make(chan struct{})}
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			b.capsCalls.Add(1)
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			b.runCalls.Add(1)
			defer b.endOnce.Do(func() { close(b.ended) })
			return h(srv, &recordingServerStream{ServerStream: ss, b: b})
		}),
	)
	fake.Register(srv)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	opts2 := append(DialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			cs, err := streamer(ctx, desc, cc, method, opts...)
			return &countingClientStream{ClientStream: cs, b: b}, err
		}),
	)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts2...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	b.client = backendv1.NewDiagnosticServiceClient(conn)
	return b
}

type recordingServerStream struct {
	grpc.ServerStream
	b *backend
}

func (s *recordingServerStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if req, ok := m.(*backendv1.RunDiagnosticRequest); ok && err == nil {
		s.b.mu.Lock()
		s.b.requests = append(s.b.requests, req)
		s.b.mu.Unlock()
	}
	return err
}

type countingClientStream struct {
	grpc.ClientStream
	b *backend
}

func (s *countingClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == nil {
		s.b.received.Add(1)
	}
	return err
}

// waitEnded fails the test unless the fake's RunDiagnostic handler returned
// within d.
func (b *backend) waitEnded(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-b.ended:
	case <-time.After(d):
		t.Fatalf("backend call still running after %s", d)
	}
}

func fakeOptions() fakebackend.Options {
	opts := fakebackend.DefaultOptions()
	opts.OutcomeDelay = time.Millisecond
	return opts
}

func newAdapter(t *testing.T, client backendv1.DiagnosticServiceClient, mutate ...func(*Config)) *Adapter {
	t.Helper()
	cfg := Config{Client: client, Log: logr.Discard()}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func session(d v1alpha.Diagnostic) Session {
	opts := fakebackend.DefaultOptions()
	return Session{
		UID:        "6f1c1b7e-0000-4000-8000-000000000001",
		ProjectID:  testProject,
		Cell:       opts.Cell,
		Location:   opts.Location,
		Diagnostic: d,
		StartedAt:  time.Now(),
	}
}

func pingDiagnostic(dest string) v1alpha.Diagnostic {
	return v1alpha.Diagnostic{Type: v1alpha.DiagnosticPing, Ping: &v1alpha.PingDiagnostic{Destination: v1alpha.ProbeDestination(dest)}}
}

// sink records the frames it receives. Its frames channel signals each one.
type sink struct {
	mu     sync.Mutex
	frames [][]byte
	notify chan string
}

func newSink() *sink {
	return &sink{notify: make(chan string, 64)}
}

func (s *sink) WriteFrame(_ context.Context, frame []byte) error {
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	s.mu.Unlock()
	var f struct{ Type string }
	_ = json.Unmarshal(frame, &f)
	select {
	case s.notify <- f.Type:
	default:
	}
	return nil
}

// waitFor blocks until a frame of type typ arrives.
func (s *sink) waitFor(t *testing.T, typ string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-s.notify:
			if got == typ {
				return
			}
		case <-timeout:
			t.Fatalf("no %s frame within 5s", typ)
		}
	}
}

// decoded returns the frames as generic JSON objects.
func (s *sink) decoded(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.frames))
	for _, f := range s.frames {
		var m map[string]any
		if err := json.Unmarshal(f, &m); err != nil {
			t.Fatalf("frame is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func frameTypes(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f["type"].(string))
	}
	return out
}

// blockingSink never completes a write until ctx ends.
type blockingSink struct{ writes atomic.Int32 }

func (s *blockingSink) WriteFrame(ctx context.Context, _ []byte) error {
	s.writes.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// scripted is a backend client whose RunDiagnostic replays a script built
// from the request.
type scripted struct {
	apiMajor uint32
	capsErr  error
	runErr   error
	script   func(req *backendv1.RunDiagnosticRequest) ([]*backendv1.DiagnosticEvent, error)

	runCalls atomic.Int32
}

func (s *scripted) GetCapabilities(context.Context, *backendv1.GetCapabilitiesRequest, ...grpc.CallOption) (*backendv1.GetCapabilitiesResponse, error) {
	if s.capsErr != nil {
		return nil, s.capsErr
	}
	major := s.apiMajor
	if major == 0 {
		major = backendv1.APIMajor
	}
	return &backendv1.GetCapabilitiesResponse{ApiMajor: major}, nil
}

func (s *scripted) RunDiagnostic(ctx context.Context, req *backendv1.RunDiagnosticRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[backendv1.DiagnosticEvent], error) {
	s.runCalls.Add(1)
	if s.runErr != nil {
		return nil, s.runErr
	}
	events, end := s.script(req)
	return &scriptedStream{ctx: ctx, events: events, end: end}, nil
}

type scriptedStream struct {
	grpc.ClientStream
	ctx    context.Context
	events []*backendv1.DiagnosticEvent
	end    error
}

func (s *scriptedStream) Recv() (*backendv1.DiagnosticEvent, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if len(s.events) == 0 {
		if s.end != nil {
			return nil, s.end
		}
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

// numbered sets the request ID and consecutive sequence numbers on events.
func numbered(req *backendv1.RunDiagnosticRequest, events ...*backendv1.DiagnosticEvent) []*backendv1.DiagnosticEvent {
	for i, ev := range events {
		ev.RequestId = req.GetRequestId()
		ev.Sequence = uint64(i + 1)
	}
	return events
}

func snapshotEvent(routers ...string) *backendv1.DiagnosticEvent {
	snap := &backendv1.Snapshot{SnapshotId: "snap-1", CapturedAt: timestamppb.Now()}
	for _, r := range routers {
		snap.Routers = append(snap.Routers, &backendv1.SnapshotRouter{Router: r, Selected: true})
	}
	return &backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Snapshot{Snapshot: snap}}
}

func observationEvent(router string, obs *backendv1.Observation) *backendv1.DiagnosticEvent {
	if obs.SampleTime == nil {
		obs.SampleTime = timestamppb.Now()
	}
	if obs.Asn == 0 {
		obs.Asn = 64496
	}
	return &backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: &backendv1.RouterOutcome{
		Router: router, CompletedAt: timestamppb.Now(),
		Result: &backendv1.RouterOutcome_Observation{Observation: obs},
	}}}
}

func failureEvent(router, reason string) *backendv1.DiagnosticEvent {
	return &backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Outcome{Outcome: &backendv1.RouterOutcome{
		Router: router, CompletedAt: timestamppb.Now(),
		Result: &backendv1.RouterOutcome_Failure{Failure: &backendv1.Failure{Reason: reason, Message: "synthetic"}},
	}}}
}

func completeEvent(c *backendv1.Complete) *backendv1.DiagnosticEvent {
	if c.SnapshotId == "" {
		c.SnapshotId = "snap-1"
	}
	c.FinishedAt = timestamppb.Now()
	return &backendv1.DiagnosticEvent{Event: &backendv1.DiagnosticEvent_Complete{Complete: c}}
}

func pingObservation() *backendv1.Observation {
	return &backendv1.Observation{Result: &backendv1.Observation_Ping{Ping: &backendv1.PingResult{
		Source: "192.0.2.1", Destination: "198.51.100.7", Sent: 3, Received: 3,
	}}}
}
