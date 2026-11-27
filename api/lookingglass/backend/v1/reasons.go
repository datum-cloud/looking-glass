package backendv1

import (
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// APIMajor is the API major this package defines. A request for any other
// major is refused with ReasonVersionUnsupported.
const APIMajor = 1

// ErrorDomain is the google.rpc.ErrorInfo domain of every refusal.
const ErrorDomain = "lookingglass.datumapis.com"

// Reasons a request is refused before the snapshot. They travel as the
// ErrorInfo reason of the gRPC status.
const (
	ReasonUnauthorized       = "Unauthorized"
	ReasonVersionUnsupported = "VersionUnsupported"
	ReasonInvalidQuery       = "InvalidQuery"
	ReasonServiceDisabled    = "ServiceDisabled"
	ReasonLocationNotServed  = "LocationNotServed"
	ReasonDuplicateRequest   = "DuplicateRequest"
	ReasonInventoryTooLarge  = "InventoryTooLarge"
	ReasonRateLimited        = "RateLimited"
)

// Reasons that appear both as refusals and as router failures.
const (
	ReasonDestinationNotAllowed  = "DestinationNotAllowed"
	ReasonQueryTypeUnavailable   = "QueryTypeUnavailable"
	ReasonProbeSourceUnavailable = "ProbeSourceUnavailable"
	ReasonRouterUnreachable      = "RouterUnreachable"
	ReasonRouterUnavailable      = "RouterUnavailable"
	ReasonQueueFull              = "QueueFull"
	ReasonQueueTimeout           = "QueueTimeout"
	ReasonDeadlineExceeded       = "DeadlineExceeded"
	ReasonResponseTooLarge       = "ResponseTooLarge"
	ReasonCancelled              = "Cancelled"
	ReasonInternal               = "Internal"
)

// Reasons that appear only as router failures.
const (
	ReasonRouterBusy = "RouterBusy"
	ReasonTimeout    = "Timeout"
	ReasonParseError = "ParseError"
)

// Omission reasons for a router a snapshot did not select.
const (
	OmittedNotRequested = "NotRequested"
	OmittedIneligible   = "Ineligible"
)

// Terminal reasons a Complete event carries.
const (
	CompleteSucceeded           = "Succeeded"
	CompletePartialResults      = "PartialResults"
	CompleteExecutionFailed     = "ExecutionFailed"
	CompleteNoNodesAvailable    = "NoNodesAvailable"
	CompleteDeadlineExceeded    = "DeadlineExceeded"
	CompleteCancelled           = "Cancelled"
	CompleteOutputLimitExceeded = "OutputLimitExceeded"
)

// Refuse returns a gRPC status error with code and message that carries
// reason as its ErrorInfo reason in ErrorDomain.
func Refuse(code codes.Code, reason, message string) error {
	st, err := status.New(code, message).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain})
	if err != nil {
		return status.Error(codes.Internal, "encoding error details")
	}
	return st.Err()
}

// RefusalReason returns the ErrorInfo reason in ErrorDomain that err
// carries, and false when err carries none.
func RefusalReason(err error) (string, bool) {
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return "", false
	}
	for _, d := range se.GRPCStatus().Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == ErrorDomain {
			return info.GetReason(), true
		}
	}
	return "", false
}

// RefuseAfter is Refuse for a throttled request: the status also carries
// retryAfter as a google.rpc.RetryInfo, advice for a new attempt.
func RefuseAfter(code codes.Code, reason, message string, retryAfter time.Duration) error {
	st, err := status.New(code, message).WithDetails(
		&errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain},
		&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)},
	)
	if err != nil {
		return status.Error(codes.Internal, "encoding error details")
	}
	return st.Err()
}

// RefusalRetryAfter returns the RetryInfo delay err carries, and false when
// it carries none.
func RefusalRetryAfter(err error) (time.Duration, bool) {
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return 0, false
	}
	for _, d := range se.GRPCStatus().Details() {
		if info, ok := d.(*errdetails.RetryInfo); ok {
			return info.GetRetryDelay().AsDuration(), true
		}
	}
	return 0, false
}
