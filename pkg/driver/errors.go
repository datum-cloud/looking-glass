package driver

import (
	"context"
	"errors"
	"fmt"
)

// Reason classifies a driver failure. Each value is the backend reason code
// the failure is reported with.
type Reason string

// The reasons a driver reports.
const (
	// ReasonRouterUnreachable: the driver could not connect to the router.
	ReasonRouterUnreachable Reason = "RouterUnreachable"
	// ReasonRouterUnavailable: the router answered but cannot serve requests,
	// for example because it refused the driver's session.
	ReasonRouterUnavailable Reason = "RouterUnavailable"
	// ReasonRouterBusy: the router refused the request for load.
	ReasonRouterBusy Reason = "RouterBusy"
	// ReasonTimeout: the request's context deadline passed.
	ReasonTimeout Reason = "Timeout"
	// ReasonCancelled: the request's context was cancelled.
	ReasonCancelled Reason = "Cancelled"
	// ReasonQueryTypeUnavailable: the router does not serve this diagnostic
	// or family.
	ReasonQueryTypeUnavailable Reason = "QueryTypeUnavailable"
	// ReasonProbeSourceUnavailable: the router has no usable source address
	// for the probe's family.
	ReasonProbeSourceUnavailable Reason = "ProbeSourceUnavailable"
	// ReasonDestinationNotAllowed: the router itself refused the destination.
	ReasonDestinationNotAllowed Reason = "DestinationNotAllowed"
	// ReasonResponseTooLarge: the router's response exceeded the driver's
	// bound.
	ReasonResponseTooLarge Reason = "ResponseTooLarge"
	// ReasonParseError: the router's response could not be parsed.
	ReasonParseError Reason = "ParseError"
	// ReasonInternal: any other failure.
	ReasonInternal Reason = "Internal"
)

// Error is a typed driver failure. Message must be safe to show an operator:
// it never contains credentials or raw router output.
type Error struct {
	Reason  Reason
	Message string
	// Err is the underlying cause, for logs only.
	Err error
}

// Errorf returns an *Error with reason and a formatted message.
func Errorf(reason Reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Reason, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf classifies err. An *Error anywhere in the chain gives its Reason.
// Otherwise a context deadline gives ReasonTimeout, a cancellation gives
// ReasonCancelled, and anything else ReasonInternal.
func ReasonOf(err error) Reason {
	var de *Error
	switch {
	case errors.As(err, &de):
		return de.Reason
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, context.Canceled):
		return ReasonCancelled
	default:
		return ReasonInternal
	}
}
