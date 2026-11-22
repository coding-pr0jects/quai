package exitkind

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		ok    bool
		facts Facts
		want  Kind
	}{
		{"pass", true, Facts{}, Pass},
		{"pass ignores stale facts", true, Facts{StepFailed: true, TimedOut: true}, Pass},
		{"step failed", false, Facts{StepFailed: true}, StepFailed},
		{"step timeout", false, Facts{TimedOut: true}, StepTimeout},
		{"lock timeout", false, Facts{LockTimedOut: true}, LockTimeout},
		{"oom killed", false, Facts{KilledBySIGKILL: true}, OOMKilled},
		{"dirty tree", false, Facts{DirtyTree: true}, DirtyTree},
		{"unexplained failure is infra", false, Facts{}, InfraError},
		{"lock timeout wins over step failure", false, Facts{LockTimedOut: true, StepFailed: true}, LockTimeout},
		{"oom wins over timeout", false, Facts{KilledBySIGKILL: true, TimedOut: true}, OOMKilled},
		{"timeout wins over dirty tree", false, Facts{TimedOut: true, DirtyTree: true}, StepTimeout},
		{"dirty tree wins over step failure", false, Facts{DirtyTree: true, StepFailed: true}, DirtyTree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.ok, tc.facts); got != tc.want {
				t.Fatalf("Classify(%v, %+v) = %q, want %q", tc.ok, tc.facts, got, tc.want)
			}
		})
	}
}
