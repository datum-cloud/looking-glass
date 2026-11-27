package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lookingglassv1alpha "go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/placement"
)

// placementRetry is how often an unplaced session asks placement again before
// its placement deadline, so a cell that becomes Ready is picked up.
const placementRetry = 5 * time.Second

// Reconciler owns a LookingGlassSession from admission to deletion, except
// for the work its cell does. It derives every decision from the object and
// the clock, so a restart or a replayed reconcile writes nothing new.
//
// Revocation (delete before terminal) and status reflection from the cell
// belong to the propagation controller (#8), which uses MergeCellStatus.
type Reconciler struct {
	Client   client.Client
	Placer   placement.Placer
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// SetupWithManager registers the reconciler with mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("looking-glass-session")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("lookingglasssession").
		For(&lookingglassv1alpha.LookingGlassSession{}).
		Complete(r)
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s lookingglassv1alpha.LookingGlassSession
	if err := r.Client.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	now := r.Clock.Now()

	if IsTerminal(&s) {
		return r.retain(ctx, &s, now)
	}

	if SetPending(&s, now) {
		if err := r.updateStatus(ctx, &s); err != nil {
			return ctrl.Result{}, err
		}
	}

	ready := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	if rank(ready) == 0 {
		return r.place(ctx, &s, now)
	}

	if deadline, ok := AgentLostDeadline(&s); ok {
		if !now.Before(deadline) {
			return r.terminate(ctx, &s, lookingglassv1alpha.ReasonAgentLost,
				"The cell stopped reporting before the session ended.", now)
		}
		return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

// place binds an unbound session to its cell, and ends a session that has
// not become SessionReady by its placement deadline.
func (r *Reconciler) place(ctx context.Context, s *lookingglassv1alpha.LookingGlassSession, now time.Time) (ctrl.Result, error) {
	deadline := PlacementDeadline(s)
	if !now.Before(deadline) {
		return r.terminate(ctx, s, lookingglassv1alpha.ReasonUnavailable,
			"No cell took the session within 30 s.", now)
	}
	untilDeadline := deadline.Sub(now)
	if s.Status.Cell != "" {
		// Bound: the cell reports SessionReady through status reflection.
		return ctrl.Result{RequeueAfter: untilDeadline}, nil
	}

	cell, err := r.Placer.CellFor(ctx, s.Spec.Location)
	if err != nil {
		// Retry on a fixed schedule rather than the workqueue's backoff, so
		// an unreadable placement source cannot push Unavailable past the
		// placement deadline.
		retry := ctrl.Result{RequeueAfter: min(placementRetry, untilDeadline)}
		if !errors.Is(err, placement.ErrNotPlaceable) {
			log.FromContext(ctx).Error(err, "reading placement source", "location", s.Spec.Location)
			return retry, nil
		}
		log.FromContext(ctx).Info("session not placeable yet", "location", s.Spec.Location, "reason", err.Error())
		r.Recorder.Eventf(s, nil, corev1.EventTypeWarning, "NotPlaceable", "Place",
			"%s; retrying until the placement deadline.", err.Error())
		return retry, nil
	}

	// A status update carries the resourceVersion read above, so a
	// concurrent writer fails it and the binding is decided once.
	s.Status.Cell = cell
	if err := r.updateStatus(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("session bound", "cell", cell)
	return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
}

// terminate ends the session with reason and schedules its deletion.
func (r *Reconciler) terminate(ctx context.Context, s *lookingglassv1alpha.LookingGlassSession, reason, message string, now time.Time) (ctrl.Result, error) {
	if Terminate(s, reason, message, now) {
		if err := r.updateStatus(ctx, s); err != nil {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).Info("session ended", "reason", reason)
	}
	return ctrl.Result{RequeueAfter: s.Status.ExpiresAt.Sub(now)}, nil
}

// retain fills in the retention timestamps of a terminal session, and
// deletes it once ExpiresAt passes.
func (r *Reconciler) retain(ctx context.Context, s *lookingglassv1alpha.LookingGlassSession, now time.Time) (ctrl.Result, error) {
	// A terminal reason written without FinishedAt finished when Ready
	// changed, bounded by creation and now. Once written it stays the same
	// across reconciles.
	ready := meta.FindStatusCondition(s.Status.Conditions, lookingglassv1alpha.ConditionReady)
	finishedAt := clamp(ready.LastTransitionTime.Time, s.CreationTimestamp.Time, now)
	if SetRetention(s, metav1.NewTime(finishedAt)) {
		if err := r.updateStatus(ctx, s); err != nil {
			return ctrl.Result{}, err
		}
	}
	if now.Before(s.Status.ExpiresAt.Time) {
		return ctrl.Result{RequeueAfter: s.Status.ExpiresAt.Sub(now)}, nil
	}
	uid := s.UID
	err := r.Client.Delete(ctx, s, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting expired session: %w", err)
	}
	if err == nil {
		log.FromContext(ctx).Info("expired session deleted", "expiresAt", s.Status.ExpiresAt.Format(time.RFC3339))
	}
	return ctrl.Result{}, nil
}

// updateStatus writes the status subresource with optimistic concurrency. A
// conflict requeues the request, which re-reads the object.
func (r *Reconciler) updateStatus(ctx context.Context, s *lookingglassv1alpha.LookingGlassSession) error {
	if err := r.Client.Status().Update(ctx, s); err != nil {
		return fmt.Errorf("updating session status: %w", err)
	}
	return nil
}
