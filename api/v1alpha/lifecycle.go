package v1alpha

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the only condition type a session carries. Its reason
// drives clients.
const ConditionReady = "Ready"

// Reasons while the session is not terminal.
const (
	// ReasonPending means placement is in progress (Ready=Unknown).
	ReasonPending = "Pending"
	// ReasonSessionReady means the client may connect before ConnectBefore
	// (Ready=True).
	ReasonSessionReady = "SessionReady"
	// ReasonConnected means the gateway claimed the session (Ready=True).
	ReasonConnected = "Connected"
)

// Terminal result reasons (Ready=False).
const (
	ReasonSucceeded           = "Succeeded"
	ReasonPartialResults      = "PartialResults"
	ReasonExecutionFailed     = "ExecutionFailed"
	ReasonNoNodesAvailable    = "NoNodesAvailable"
	ReasonOutputLimitExceeded = "OutputLimitExceeded"
)

// Terminal session failure reasons (Ready=False).
const (
	ReasonUnavailable      = "Unavailable"
	ReasonRefused          = "Refused"
	ReasonThrottled        = "Throttled"
	ReasonNotConnected     = "NotConnected"
	ReasonDeadlineExceeded = "DeadlineExceeded"
	ReasonRevoked          = "Revoked"
	ReasonDisconnected     = "Disconnected"
	ReasonClosedByUser     = "ClosedByUser"
	ReasonAgentLost        = "AgentLost"
	ReasonBackendError     = "BackendError"
)

// SessionState is the client-facing lifecycle state derived from the Ready
// condition.
type SessionState string

const (
	// SessionStatePending means the client keeps watching.
	SessionStatePending SessionState = "Pending"
	// SessionStateConnectable means the client connects before ConnectBefore.
	SessionStateConnectable SessionState = "Connectable"
	// SessionStateConnected means the session is claimed and running.
	SessionStateConnected SessionState = "Connected"
	// SessionStateTerminal means the session ended; the Ready reason is final.
	SessionStateTerminal SessionState = "Terminal"
)

// State derives the lifecycle state from the Ready condition. A missing
// Ready condition, or one older than the current generation, is Pending: a
// new session has no status until the controller writes one. Any Ready=False
// is Terminal, including reasons this version does not know.
func (s *LookingGlassSession) State() SessionState {
	ready := meta.FindStatusCondition(s.Status.Conditions, ConditionReady)
	if ready == nil || ready.ObservedGeneration < s.Generation {
		return SessionStatePending
	}
	switch ready.Status {
	case metav1.ConditionFalse:
		return SessionStateTerminal
	case metav1.ConditionTrue:
		switch ready.Reason {
		case ReasonSessionReady:
			return SessionStateConnectable
		case ReasonConnected:
			return SessionStateConnected
		}
	}
	return SessionStatePending
}
