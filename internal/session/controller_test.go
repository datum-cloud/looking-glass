package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lookingglassv1alpha "go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/placement"
)

const (
	testNS       = "default"
	testLocation = "example-location"
	testKey      = "0000000000000000000000000000000000000000000000000000000000000001"
)

// k8s talks to envtest's API server. It is nil when KUBEBUILDER_ASSETS is
// unset, and the envtest tests skip.
var k8s client.Client

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
	}()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "building scheme: %v\n", err)
		return 1
	}
	if err := lookingglassv1alpha.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "building scheme: %v\n", err)
		return 1
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		return 1
	}
	return m.Run()
}

// countingPlacer records how often placement is asked.
type countingPlacer struct {
	placement.Placer
	calls atomic.Int32
}

func (p *countingPlacer) CellFor(ctx context.Context, location string) (string, error) {
	p.calls.Add(1)
	return p.Placer.CellFor(ctx, location)
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	clock  *clocktesting.FakePassiveClock
	placer *countingPlacer
	key    types.NamespacedName
	// created is the session's creationTimestamp, which the API server sets.
	created time.Time
}

func newHarness(t *testing.T, cells map[string]string) *harness {
	t.Helper()
	if k8s == nil {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `task test` to provide envtest binaries")
	}
	ctx := t.Context()
	s := &lookingglassv1alpha.LookingGlassSession{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "lg-", Namespace: testNS},
		Spec: lookingglassv1alpha.LookingGlassSessionSpec{
			Location: testLocation,
			Diagnostic: lookingglassv1alpha.Diagnostic{
				Type: lookingglassv1alpha.DiagnosticPing,
				Ping: &lookingglassv1alpha.PingDiagnostic{Destination: "192.0.2.1"},
			},
			ClientPublicKey: testKey,
		},
	}
	if err := k8s.Create(ctx, s); err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), s)
	})
	return &harness{
		t:       t,
		ctx:     ctx,
		clock:   clocktesting.NewFakePassiveClock(s.CreationTimestamp.Time),
		placer:  &countingPlacer{Placer: placement.StaticPlacer{Cells: cells}},
		key:     client.ObjectKeyFromObject(s),
		created: s.CreationTimestamp.Time,
	}
}

// reconcile runs a fresh Reconciler, as a restarted manager would, at
// created+offset.
func (h *harness) reconcile(offset time.Duration) ctrl.Result {
	h.t.Helper()
	h.clock.SetTime(h.created.Add(offset))
	r := &Reconciler{Client: k8s, Placer: h.placer, Clock: h.clock, Recorder: events.NewFakeRecorder(100)}
	res, err := r.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key})
	if err != nil {
		h.t.Fatalf("Reconcile at +%v: %v", offset, err)
	}
	return res
}

func (h *harness) get() *lookingglassv1alpha.LookingGlassSession {
	h.t.Helper()
	var s lookingglassv1alpha.LookingGlassSession
	if err := k8s.Get(h.ctx, h.key, &s); err != nil {
		h.t.Fatalf("getting session: %v", err)
	}
	return &s
}

// reportFromCell merges status from the bound cell, as status reflection
// (#8) will, and writes it.
func (h *harness) reportFromCell(in *lookingglassv1alpha.LookingGlassSessionStatus) {
	h.t.Helper()
	s := h.get()
	changed, err := MergeCellStatus(s, s.Status.Cell, in, h.clock.Now())
	if err != nil || !changed {
		h.t.Fatalf("MergeCellStatus: changed, err = %v, %v", changed, err)
	}
	if err := k8s.Status().Update(h.ctx, s); err != nil {
		h.t.Fatalf("writing cell status: %v", err)
	}
}

func (h *harness) sessionReady(connectBefore time.Time) {
	h.t.Helper()
	cb := metav1.NewTime(connectBefore)
	h.reportFromCell(&lookingglassv1alpha.LookingGlassSessionStatus{
		Conditions:    ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonSessionReady),
		ConnectBefore: &cb,
		Connection:    connection(),
	})
}

func wantReason(t *testing.T, s *lookingglassv1alpha.LookingGlassSession, want string) {
	t.Helper()
	if got := reason(s); got != want {
		t.Fatalf("reason = %q; want %q (status %+v)", got, want, s.Status)
	}
}

func TestReconcileBindsPendingSession(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	if s := h.get(); s.State() != lookingglassv1alpha.SessionStatePending || s.Status.Cell != "" {
		t.Fatalf("new session state, cell = %v, %q", s.State(), s.Status.Cell)
	}

	h.reconcile(time.Second)
	s := h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonPending)
	if s.Status.Cell != "cell-1" {
		t.Fatalf("cell = %q; want cell-1", s.Status.Cell)
	}
	if s.Status.Connection != nil || s.Status.ConnectBefore != nil {
		t.Fatalf("connection details published at bind: %+v", s.Status)
	}

	// Replays and restarts change nothing and do not ask placement again.
	rv := s.ResourceVersion
	for _, at := range []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second} {
		h.reconcile(at)
	}
	if got := h.get().ResourceVersion; got != rv {
		t.Fatalf("replayed reconciles wrote the session: resourceVersion %s -> %s", rv, got)
	}
	if n := h.placer.calls.Load(); n != 1 {
		t.Fatalf("placement asked %d times; want 1", n)
	}
}

func TestReconcileKeepsBindingWhenPlacementChanges(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	h.reconcile(time.Second)
	h.placer.Placer = placement.StaticPlacer{Cells: map[string]string{testLocation: "cell-2"}}
	h.reconcile(2 * time.Second)
	if got := h.get().Status.Cell; got != "cell-1" {
		t.Fatalf("cell = %q after placement changed; want cell-1", got)
	}
}

func TestReconcileUnavailableWithoutCell(t *testing.T) {
	h := newHarness(t, nil)
	res := h.reconcile(time.Second)
	s := h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonPending)
	if s.Status.Cell != "" {
		t.Fatalf("cell = %q; want unbound", s.Status.Cell)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 5*time.Second {
		t.Fatalf("RequeueAfter = %v; want a retry within 5 s", res.RequeueAfter)
	}

	h.reconcile(29 * time.Second)
	wantReason(t, h.get(), lookingglassv1alpha.ReasonPending)

	h.reconcile(30 * time.Second)
	s = h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonUnavailable)
	if want := h.created.Add(30 * time.Second); !s.Status.FinishedAt.Time.Equal(want) || !s.Status.ExpiresAt.Time.Equal(want.Add(15*time.Minute)) {
		t.Fatalf("FinishedAt, ExpiresAt = %v, %v; want %v and 15 m later", s.Status.FinishedAt, s.Status.ExpiresAt, want)
	}
}

func TestReconcileUnavailableWhenCellNeverReady(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	h.reconcile(time.Second)
	if res := h.reconcile(10 * time.Second); res.RequeueAfter != 20*time.Second {
		t.Fatalf("RequeueAfter = %v; want the 20 s left to the placement deadline", res.RequeueAfter)
	}
	h.reconcile(30 * time.Second)
	s := h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonUnavailable)
	if s.Status.Cell != "cell-1" {
		t.Fatalf("cell = %q; the binding must survive the terminal reason", s.Status.Cell)
	}
}

func TestReconcileAgentLostBackstop(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	h.reconcile(time.Second)
	h.clock.SetTime(h.created.Add(5 * time.Second))
	connectBefore := h.created.Add(65 * time.Second)
	h.sessionReady(connectBefore)

	// SessionReady past the placement deadline is not Unavailable.
	h.reconcile(31 * time.Second)
	wantReason(t, h.get(), lookingglassv1alpha.ReasonSessionReady)

	h.clock.SetTime(h.created.Add(70 * time.Second))
	h.reportFromCell(&lookingglassv1alpha.LookingGlassSessionStatus{
		Conditions: ready(metav1.ConditionTrue, lookingglassv1alpha.ReasonConnected),
	})

	backstop := 65*time.Second + 150*time.Second
	if res := h.reconcile(backstop - time.Second); res.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %v; want 1 s to the backstop", res.RequeueAfter)
	}
	wantReason(t, h.get(), lookingglassv1alpha.ReasonConnected)

	h.reconcile(backstop)
	s := h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonAgentLost)
	if s.Status.Connection != nil {
		t.Fatal("connection details kept after AgentLost")
	}
	if !s.Status.FinishedAt.Time.Equal(h.created.Add(backstop)) {
		t.Fatalf("FinishedAt = %v; want the backstop time", s.Status.FinishedAt)
	}

	// A delayed report from the cell does not replace the terminal reason.
	changed, err := MergeCellStatus(s, "cell-1", &lookingglassv1alpha.LookingGlassSessionStatus{
		Conditions: ready(metav1.ConditionFalse, lookingglassv1alpha.ReasonSucceeded),
	}, h.clock.Now())
	if err != nil || changed {
		t.Fatalf("late cell report: changed, err = %v, %v; want ignored", changed, err)
	}
}

func TestReconcileRetentionAndRestart(t *testing.T) {
	h := newHarness(t, nil)
	h.reconcile(30 * time.Second)
	s := h.get()
	wantReason(t, s, lookingglassv1alpha.ReasonUnavailable)
	expiresAt := s.Status.ExpiresAt.Time
	rv := s.ResourceVersion

	// A restart during retention rewrites nothing and waits for expiresAt.
	offset := expiresAt.Sub(h.created)
	if res := h.reconcile(offset - time.Second); res.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %v; want 1 s to expiresAt", res.RequeueAfter)
	}
	if got := h.get(); got.ResourceVersion != rv || !got.Status.ExpiresAt.Time.Equal(expiresAt) {
		t.Fatalf("restart during retention changed the session: rv %s -> %s, expiresAt %v -> %v",
			rv, got.ResourceVersion, expiresAt, got.Status.ExpiresAt)
	}

	h.reconcile(offset)
	err := k8s.Get(h.ctx, h.key, &lookingglassv1alpha.LookingGlassSession{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("session after expiresAt: %v; want NotFound", err)
	}
	// A reconcile of the deleted session is a no-op.
	h.reconcile(offset + time.Second)
}

func TestReconcileFillsRetentionForCellTerminalReason(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	h.reconcile(time.Second)

	// A terminal reason written without timestamps, for example by an older
	// writer, gets them from its transition time.
	s := h.get()
	s.Status.Conditions = []metav1.Condition{{
		Type:               lookingglassv1alpha.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             lookingglassv1alpha.ReasonRefused,
		Message:            "Refused by the cell.",
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.NewTime(h.created.Add(10 * time.Second)),
	}}
	if err := k8s.Status().Update(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.reconcile(20 * time.Second)
	s = h.get()
	if want := h.created.Add(10 * time.Second); !s.Status.FinishedAt.Time.Equal(want) || !s.Status.ExpiresAt.Time.Equal(want.Add(15*time.Minute)) {
		t.Fatalf("FinishedAt, ExpiresAt = %v, %v; want %v and 15 m later", s.Status.FinishedAt, s.Status.ExpiresAt, want)
	}
	wantReason(t, s, lookingglassv1alpha.ReasonRefused)
}

// failingPlacer cannot read its source.
type failingPlacer struct{}

func (failingPlacer) CellFor(context.Context, string) (string, error) {
	return "", errors.New("federation hub unreachable")
}

func TestReconcileUnreadablePlacementKeepsDeadline(t *testing.T) {
	h := newHarness(t, nil)
	h.placer.Placer = failingPlacer{}
	if res := h.reconcile(28 * time.Second); res.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v; want the 2 s left to the placement deadline", res.RequeueAfter)
	}
	wantReason(t, h.get(), lookingglassv1alpha.ReasonPending)
	h.reconcile(30 * time.Second)
	wantReason(t, h.get(), lookingglassv1alpha.ReasonUnavailable)
}

func TestBindingLosesToConcurrentWriter(t *testing.T) {
	h := newHarness(t, map[string]string{testLocation: "cell-1"})
	stale := h.get()

	// Another writer binds first.
	h.reconcile(time.Second)
	if got := h.get().Status.Cell; got != "cell-1" {
		t.Fatalf("cell = %q; want cell-1", got)
	}

	// A reconcile working from the stale copy fails its update and binds
	// nothing.
	h.clock.SetTime(h.created.Add(2 * time.Second))
	r := &Reconciler{
		Client:   k8s,
		Placer:   placement.StaticPlacer{Cells: map[string]string{testLocation: "cell-2"}},
		Clock:    h.clock,
		Recorder: events.NewFakeRecorder(10),
	}
	_, err := r.place(h.ctx, stale, h.clock.Now())
	if !apierrors.IsConflict(err) {
		t.Fatalf("place from a stale copy: %v; want a conflict", err)
	}
	if got := h.get().Status.Cell; got != "cell-1" {
		t.Fatalf("cell = %q after the stale write; want cell-1", got)
	}
}

func TestReconcileEmitsNotPlaceableEvent(t *testing.T) {
	h := newHarness(t, nil)
	rec := events.NewFakeRecorder(10)
	h.clock.SetTime(h.created.Add(time.Second))
	r := &Reconciler{Client: k8s, Placer: h.placer, Clock: h.clock, Recorder: rec}
	if _, err := r.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.key}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-rec.Events:
		if want := corev1.EventTypeWarning + " NotPlaceable"; len(e) < len(want) || e[:len(want)] != want {
			t.Fatalf("event = %q; want prefix %q", e, want)
		}
	default:
		t.Fatal("no NotPlaceable event")
	}
}
