// Package exitkind normalizes why a quai command did not pass.
package exitkind

type Kind string

const (
	Pass        Kind = "pass"
	StepFailed  Kind = "step_failed"
	StepTimeout Kind = "step_timeout"
	LockTimeout Kind = "lock_timeout"
	OOMKilled   Kind = "oom_killed"
	DirtyTree   Kind = "dirty_tree"
	InfraError  Kind = "infra_error"
)

// Facts records the failure signals observed during one command run.
type Facts struct {
	TimedOut        bool
	KilledBySIGKILL bool
	StepFailed      bool
	DirtyTree       bool
	LockTimedOut    bool
}

// Classify maps observed facts to one Kind. An unexplained failure is an
// InfraError, never a Pass, because a failure is never reported as success.
func Classify(ok bool, f Facts) Kind {
	switch {
	case ok:
		return Pass
	case f.LockTimedOut:
		return LockTimeout
	case f.KilledBySIGKILL:
		return OOMKilled
	case f.TimedOut:
		return StepTimeout
	case f.DirtyTree:
		return DirtyTree
	case f.StepFailed:
		return StepFailed
	default:
		return InfraError
	}
}
