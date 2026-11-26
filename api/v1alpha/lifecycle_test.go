package v1alpha

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestState(t *testing.T) {
	ready := func(status metav1.ConditionStatus, reason string, gen int64) []metav1.Condition {
		return []metav1.Condition{{Type: ConditionReady, Status: status, Reason: reason, ObservedGeneration: gen}}
	}
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       SessionState
	}{
		{"no status yet", nil, SessionStatePending},
		{"other condition only", []metav1.Condition{{Type: "Other", Status: metav1.ConditionTrue, ObservedGeneration: 1}}, SessionStatePending},
		{"pending", ready(metav1.ConditionUnknown, ReasonPending, 1), SessionStatePending},
		{"session ready", ready(metav1.ConditionTrue, ReasonSessionReady, 1), SessionStateConnectable},
		{"connected", ready(metav1.ConditionTrue, ReasonConnected, 1), SessionStateConnected},
		{"succeeded", ready(metav1.ConditionFalse, ReasonSucceeded, 1), SessionStateTerminal},
		{"session failure", ready(metav1.ConditionFalse, ReasonNotConnected, 1), SessionStateTerminal},
		{"unknown terminal reason", ready(metav1.ConditionFalse, "SomethingNew", 1), SessionStateTerminal},
		{"unknown ready reason", ready(metav1.ConditionTrue, "SomethingNew", 1), SessionStatePending},
		{"stale generation", ready(metav1.ConditionFalse, ReasonSucceeded, 0), SessionStatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &LookingGlassSession{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status:     LookingGlassSessionStatus{Conditions: tt.conditions},
			}
			if got := s.State(); got != tt.want {
				t.Errorf("State() = %q, want %q", got, tt.want)
			}
		})
	}
}
