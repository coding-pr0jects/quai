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
	Refused     Kind = "refused"
	InfraError  Kind = "infra_error"
)

// Facts records the failure signals observed during one command run.
type Facts struct {
	TimedOut        bool
	KilledBySIGKILL bool
	StepFailed      bool
	DirtyTree       bool
	LockTimedOut    bool
	Refused         bool
}

// severity ranks kinds from the cleanest to the one that most needs attention.
// The order is part of the batch exit_kind contract.
func severity(k Kind) int {
	switch k {
	case Pass:
		return 0
	case Refused:
		return 1
	case StepFailed:
		return 2
	case DirtyTree:
		return 3
	case StepTimeout:
		return 4
	case OOMKilled:
		return 5
	case LockTimeout:
		return 6
	default:
		return 7
	}
}

// Worst returns the highest-severity kind, or Pass when no kind is given.
func Worst(kinds ...Kind) Kind {
	worst := Pass
	for _, k := range kinds {
		if severity(k) == 7 {
			k = InfraError
		}
		if severity(k) > severity(worst) {
			worst = k
		}
	}
	return worst
}

// Classify maps observed facts to one Kind. An unexplained failure is an
// InfraError, never a Pass, because a failure is never reported as success.
func Classify(ok bool, f Facts) Kind {
	switch {
	case ok:
		return Pass
	case f.LockTimedOut:
		return LockTimeout
	case f.Refused:
		return Refused
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
