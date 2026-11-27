package diagnostics

import (
	"context"
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	backendv1 "go.datum.net/looking-glass/api/lookingglass/backend/v1"
	"go.datum.net/looking-glass/api/v1alpha"
)

// Causes the gateway passes to context.CancelCauseFunc to stop a session. Run
// reads the cause with context.Cause and ends the session with the matching
// terminal reason.
var (
	// ErrDisconnected means the client connection was lost without a close.
	ErrDisconnected = errors.New("client connection lost")
	// ErrClosedByUser means the client closed the stream with code 1000.
	ErrClosedByUser = errors.New("client closed the stream")
	// ErrRevoked means the session was deleted or revoked.
	ErrRevoked = errors.New("session revoked")
	// ErrShutdown means the gateway is shutting down.
	ErrShutdown = errors.New("gateway shutting down")
	// ErrSlowClient means the client did not read frames fast enough and the
	// gateway disconnected it. Run sets it itself.
	ErrSlowClient = errors.New("client too slow to read the stream")

	errDeadline = errors.New("execution deadline passed")
)

// Reasons a router failure frame carries.
const (
	FailureUnavailable      = "Unavailable"
	FailureRefused          = "Refused"
	FailureTimeout          = "Timeout"
	FailureThrottled        = "Throttled"
	FailureUnsupported      = "Unsupported"
	FailureResponseTooLarge = "ResponseTooLarge"
	FailureCancelled        = "Cancelled"
	FailureBackendError     = "BackendError"
)

// failureReason maps a backend router failure reason to the failure frame's
// reason. An unknown reason is a backend error, never packet loss.
func failureReason(backend string) string {
	switch backend {
	case backendv1.ReasonRouterUnreachable, backendv1.ReasonRouterUnavailable:
		return FailureUnavailable
	case backendv1.ReasonDestinationNotAllowed, backendv1.ReasonInvalidQuery:
		return FailureRefused
	case backendv1.ReasonTimeout, backendv1.ReasonQueueTimeout, backendv1.ReasonDeadlineExceeded:
		return FailureTimeout
	case backendv1.ReasonRateLimited, backendv1.ReasonQueueFull, backendv1.ReasonRouterBusy:
		return FailureThrottled
	case backendv1.ReasonQueryTypeUnavailable, backendv1.ReasonProbeSourceUnavailable:
		return FailureUnsupported
	case backendv1.ReasonResponseTooLarge:
		return FailureResponseTooLarge
	case backendv1.ReasonCancelled:
		return FailureCancelled
	default:
		return FailureBackendError
	}
}

// refusalReason maps a backend reason received before the snapshot to the
// session's terminal reason. Cancelled is handled by the caller.
func refusalReason(backend string) string {
	switch backend {
	case backendv1.ReasonUnauthorized, backendv1.ReasonVersionUnsupported, backendv1.ReasonQueryTypeUnavailable,
		backendv1.ReasonInvalidQuery, backendv1.ReasonDestinationNotAllowed:
		return v1alpha.ReasonRefused
	case backendv1.ReasonServiceDisabled, backendv1.ReasonLocationNotServed, backendv1.ReasonRouterUnreachable,
		backendv1.ReasonRouterUnavailable, backendv1.ReasonProbeSourceUnavailable:
		return v1alpha.ReasonUnavailable
	case backendv1.ReasonRateLimited, backendv1.ReasonQueueFull:
		return v1alpha.ReasonThrottled
	case backendv1.ReasonInventoryTooLarge, backendv1.ReasonResponseTooLarge, backendv1.CompleteOutputLimitExceeded:
		return v1alpha.ReasonOutputLimitExceeded
	case backendv1.ReasonDeadlineExceeded, backendv1.ReasonQueueTimeout:
		return v1alpha.ReasonDeadlineExceeded
	default:
		return v1alpha.ReasonBackendError
	}
}

// completeReason maps a Complete event's reason to the session's terminal
// reason. It returns "" for Cancelled, whose meaning depends on the cause the
// gateway knows.
func completeReason(backend string) string {
	switch backend {
	case backendv1.CompleteSucceeded:
		return v1alpha.ReasonSucceeded
	case backendv1.CompletePartialResults:
		return v1alpha.ReasonPartialResults
	case backendv1.CompleteExecutionFailed:
		return v1alpha.ReasonExecutionFailed
	case backendv1.CompleteNoNodesAvailable:
		return v1alpha.ReasonNoNodesAvailable
	case backendv1.CompleteDeadlineExceeded:
		return v1alpha.ReasonDeadlineExceeded
	case backendv1.CompleteOutputLimitExceeded:
		return v1alpha.ReasonOutputLimitExceeded
	case backendv1.CompleteCancelled:
		return ""
	default:
		return v1alpha.ReasonBackendError
	}
}

// causeReason returns the terminal reason for the cause that stopped ctx, or
// "" when ctx is not done or its cause is not one the gateway knows.
// snapshot reports whether the backend's snapshot had arrived: a shutdown
// before it means the backend was unavailable, after it the session's agent
// was lost.
func causeReason(ctx context.Context, snapshot bool) string {
	if ctx.Err() == nil {
		return ""
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrRevoked):
		return v1alpha.ReasonRevoked
	case errors.Is(cause, errDeadline), errors.Is(cause, context.DeadlineExceeded):
		return v1alpha.ReasonDeadlineExceeded
	case errors.Is(cause, ErrShutdown):
		if snapshot {
			return v1alpha.ReasonAgentLost
		}
		return v1alpha.ReasonUnavailable
	case errors.Is(cause, ErrDisconnected), errors.Is(cause, ErrSlowClient):
		return v1alpha.ReasonDisconnected
	case errors.Is(cause, ErrClosedByUser):
		return v1alpha.ReasonClosedByUser
	default:
		return ""
	}
}

// callErrorReason maps an error from a backend call made before the snapshot
// (GetCapabilities, or RunDiagnostic before its first event) to the session's
// terminal reason and the backend's retry advice. A cause the gateway knows
// wins over whatever the call returned.
func callErrorReason(ctx context.Context, err error) (string, *time.Duration) {
	if r := causeReason(ctx, false); r != "" {
		return r, nil
	}
	if backend, ok := backendv1.RefusalReason(err); ok {
		if backend == backendv1.ReasonCancelled {
			return v1alpha.ReasonUnavailable, nil
		}
		reason := refusalReason(backend)
		if reason == v1alpha.ReasonThrottled {
			return reason, retryAfter(err)
		}
		return reason, nil
	}
	// No backend reason: the call failed in transport, TLS or identity.
	switch status.Code(err) {
	case codes.Unavailable, codes.Unauthenticated, codes.PermissionDenied, codes.Canceled:
		return v1alpha.ReasonUnavailable, nil
	case codes.DeadlineExceeded:
		return v1alpha.ReasonDeadlineExceeded, nil
	default:
		return v1alpha.ReasonBackendError, nil
	}
}

// retryAfter returns the delay a google.rpc.RetryInfo detail on err advises,
// if any.
func retryAfter(err error) *time.Duration {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.RetryInfo); ok && info.GetRetryDelay() != nil {
			delay := info.GetRetryDelay().AsDuration()
			return &delay
		}
	}
	return nil
}

// endMessages are the end frame's messages. They are fixed text: nothing a
// backend or router returned reaches the client through them.
var endMessages = map[string]string{
	v1alpha.ReasonSucceeded:           "Every selected router answered.",
	v1alpha.ReasonPartialResults:      "Some routers did not answer, or answered in part.",
	v1alpha.ReasonExecutionFailed:     "No selected router answered.",
	v1alpha.ReasonNoNodesAvailable:    "No router at the location could run the diagnostic.",
	v1alpha.ReasonOutputLimitExceeded: "The result exceeded the output limit.",
	v1alpha.ReasonUnavailable:         "The diagnostic service at the location is unavailable.",
	v1alpha.ReasonRefused:             "The location refused the diagnostic.",
	v1alpha.ReasonThrottled:           "The location is busy. Try again later.",
	v1alpha.ReasonDeadlineExceeded:    "The diagnostic ran past its deadline.",
	v1alpha.ReasonRevoked:             "The session was revoked.",
	v1alpha.ReasonAgentLost:           "The gateway stopped before the diagnostic finished.",
	v1alpha.ReasonBackendError:        "The diagnostic service failed.",
}
