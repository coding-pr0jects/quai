// Package detach runs a quai command in the background as a detached run, stored under
// <git-common-dir>/quai/runs/<run-id>/, and lets later calls wait for it, stream its output,
// and stop it.
package detach

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/execrun"
	"github.com/coding-pr0jects/quai/internal/lock"
)

// EnvRunID tells a quai process that it is the detached side of the run with this ID.
const EnvRunID = "QUAI_RUN_ID"

const (
	metaVersion  = 1
	keepFinished = 20
	pollInterval = 200 * time.Millisecond

	// StopGrace outlasts the grace execrun gives a step after SIGTERM, so a stopped run can
	// still kill its steps and release its locks before it gets SIGKILL.
	StopGrace = 10 * time.Second
)

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// ErrNoRun means no detached run matches the request.
var ErrNoRun = errors.New("no detached run found")

// Meta describes a detached run. It is written once, when the run starts.
type Meta struct {
	Version   int       `json:"version"`
	ID        string    `json:"run_id"`
	Args      []string  `json:"args"`
	Worktree  string    `json:"worktree"`
	PID       int       `json:"pid"`
	StartTime uint64    `json:"start_time,omitempty"`
	Host      string    `json:"host,omitempty"`
	PIDNS     string    `json:"pid_ns,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Exit is the record a detached run leaves when it finishes.
type Exit struct {
	ExitCode   int             `json:"exit_code"`
	FinishedAt time.Time       `json:"finished_at"`
	Stopped    bool            `json:"stopped,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

// State is where a detached run stands: running, passed, failed, or dead (gone without an exit record).
type State string

const (
	Running State = "running"
	Passed  State = "passed"
	Failed  State = "failed"
	Dead    State = "dead"
)

// Status is what quai reports about a detached run.
type Status struct {
	Meta
	State   State  `json:"state"`
	LogPath string `json:"log_path"`
	Exit    *Exit  `json:"exit,omitempty"`
}

// Report is the outcome of a wait or a stop.
type Report struct {
	Status
	Output        string   `json:"output"`
	ReleasedLocks []string `json:"released_locks,omitempty"`
	KilledGroups  []int    `json:"killed_groups,omitempty"`
}

// ExitCode maps a run's state to the exit code of quai verify --wait: 0 passed, 1 failed or
// dead, 4 still running.
func ExitCode(s State) int {
	switch s {
	case Passed:
		return 0
	case Running:
		return 4
	default:
		return 1
	}
}

func decideState(exit *Exit, probeable, alive bool) State {
	switch {
	case exit != nil && exit.ExitCode == 0:
		return Passed
	case exit != nil:
		return Failed
	case !probeable || alive:
		return Running
	default:
		return Dead
	}
}

// Store holds the detached runs of one repository.
type Store struct {
	root      string
	locksRoot string
}

// NewStore opens the detached runs under commonDir/quai/runs.
func NewStore(commonDir string) *Store {
	return &Store{
		root:      filepath.Join(commonDir, "quai", "runs"),
		locksRoot: filepath.Join(commonDir, "quai", "locks"),
	}
}

func (s *Store) dir(id string) string { return filepath.Join(s.root, id) }

// Start launches the quai executable with args (a subcommand and its flags) as a detached run
// in dir, records it, and prunes old finished runs.
func (s *Store) Start(dir, executable string, args []string, worktree string) (Meta, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return Meta{}, fmt.Errorf("create runs dir: %w", err)
	}
	id, err := newRunID()
	if err != nil {
		return Meta{}, err
	}
	runDir := s.dir(id)
	if err := os.Mkdir(runDir, 0o755); err != nil {
		return Meta{}, fmt.Errorf("create run dir: %w", err)
	}
	env := append(os.Environ(), EnvRunID+"="+id)
	proc, err := execrun.Detach(dir, append([]string{executable}, args...), env, filepath.Join(runDir, "output.log"))
	if err != nil {
		_ = os.RemoveAll(runDir)
		return Meta{}, fmt.Errorf("start detached run: %w", err)
	}
	meta := Meta{
		Version:   metaVersion,
		ID:        id,
		Args:      args,
		Worktree:  worktree,
		PID:       proc.PID,
		StartTime: proc.StartTime,
		Host:      lock.LocalHost(),
		PIDNS:     lock.LocalPIDNS(),
		StartedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(runDir, "meta.json"), meta); err != nil {
		proc.KillGroup()
		_ = os.RemoveAll(runDir)
		return Meta{}, err
	}
	s.prune()
	return meta, nil
}

// Resolve returns the run with this ID, or the latest run started from worktree when id is empty.
func (s *Store) Resolve(id, worktree string) (Meta, error) {
	if id != "" {
		if !runIDPattern.MatchString(id) {
			return Meta{}, fmt.Errorf("invalid run id %q: expected the id printed by --detach", id)
		}
		m, err := s.readMeta(id)
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, fmt.Errorf("%w: %s", ErrNoRun, id)
		}
		return m, err
	}
	metas := s.list()
	for i := len(metas) - 1; i >= 0; i-- {
		if metas[i].Worktree == worktree {
			return metas[i], nil
		}
	}
	return Meta{}, fmt.Errorf("%w from worktree %s: start one with --detach", ErrNoRun, worktree)
}

// Inspect reports the current state of a run without changing anything.
func (s *Store) Inspect(m Meta) Status {
	st := Status{Meta: m, LogPath: filepath.Join(s.dir(m.ID), "output.log")}
	var e Exit
	if readJSON(filepath.Join(s.dir(m.ID), "exit.json"), &e) == nil {
		st.Exit = &e
	}
	probeable := probeable(m)
	st.State = decideState(st.Exit, probeable, probeable && processOf(m).Alive())
	return st
}

// Active lists the runs that are running, or dead without an exit record, oldest first.
func (s *Store) Active() []Status {
	var out []Status
	for _, m := range s.list() {
		if st := s.Inspect(m); st.State == Running || st.State == Dead {
			out = append(out, st)
		}
	}
	return out
}

// Wait streams the run's output written since the previous wait to out, until the run
// finishes, max elapses, or ctx is done. A dead run has its process groups killed and its
// leftover locks released.
func (s *Store) Wait(ctx context.Context, m Meta, max time.Duration, out io.Writer) (Report, error) {
	deadline := time.Now().Add(max)
	for {
		st := s.Inspect(m)
		if err := s.copyNewOutput(m.ID, out, st.State != Running); err != nil {
			return Report{Status: st}, err
		}
		if st.State != Running {
			rep := Report{Status: st}
			if st.State == Dead {
				err := s.cleanUp(m, &rep)
				return rep, err
			}
			return rep, nil
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return Report{Status: st}, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(pollInterval, time.Until(deadline))):
		}
	}
}

// Stop sends SIGTERM to a running run's process group, then SIGKILL after StopGrace, kills
// the process groups of its steps, and releases its leftover locks.
func (s *Store) Stop(m Meta, grace time.Duration) (Report, error) {
	if !probeable(m) {
		return Report{}, fmt.Errorf("run %s belongs to host %s (pid namespace %s): stop it from there", m.ID, m.Host, m.PIDNS)
	}
	rep := Report{Status: s.Inspect(m)}
	if rep.State != Running && rep.State != Dead {
		return rep, nil
	}
	wasRunning := rep.State == Running
	processOf(m).StopGroup(grace)
	if err := s.cleanUp(m, &rep); err != nil {
		return rep, err
	}
	if wasRunning {
		e := Exit{ExitCode: 137, FinishedAt: time.Now().UTC()}
		_ = readJSON(filepath.Join(s.dir(m.ID), "exit.json"), &e)
		e.Stopped = true
		if err := writeJSONAtomic(filepath.Join(s.dir(m.ID), "exit.json"), e); err != nil {
			return rep, err
		}
	}
	rep.Status = s.Inspect(m)
	return rep, nil
}

func (s *Store) cleanUp(m Meta, rep *Report) error {
	for _, p := range s.readGroups(m.ID) {
		if p.Alive() {
			rep.KilledGroups = append(rep.KilledGroups, p.PID)
		}
		p.KillGroup()
	}
	released, err := lock.ReleaseDead(s.locksRoot, m.PID)
	rep.ReleasedLocks = released
	return err
}

func processOf(m Meta) execrun.Process {
	return execrun.Process{PID: m.PID, StartTime: m.StartTime}
}

func probeable(m Meta) bool {
	return (m.Host == "" || m.Host == lock.LocalHost()) && (m.PIDNS == "" || m.PIDNS == lock.LocalPIDNS())
}

func (s *Store) copyNewOutput(id string, out io.Writer, final bool) error {
	runDir := s.dir(id)
	f, err := os.Open(filepath.Join(runDir, "output.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	cursorPath := filepath.Join(runDir, "cursor")
	var offset int64
	if raw, err := os.ReadFile(cursorPath); err == nil {
		offset, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if !final {
		chunk = chunk[:bytes.LastIndexByte(chunk, '\n')+1]
	}
	if len(chunk) == 0 {
		return nil
	}
	if _, err := out.Write(chunk); err != nil {
		return err
	}
	return writeFileAtomic(cursorPath, []byte(strconv.FormatInt(offset+int64(len(chunk)), 10)))
}

func (s *Store) readMeta(id string) (Meta, error) {
	var m Meta
	err := readJSON(filepath.Join(s.dir(id), "meta.json"), &m)
	return m, err
}

func (s *Store) list() []Meta {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	var metas []Meta
	for _, e := range entries {
		if !e.IsDir() || !runIDPattern.MatchString(e.Name()) {
			continue
		}
		if m, err := s.readMeta(e.Name()); err == nil {
			metas = append(metas, m)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].StartedAt.Before(metas[j].StartedAt) })
	return metas
}

func (s *Store) prune() {
	metas := s.list()
	kept := 0
	for i := len(metas) - 1; i >= 0; i-- {
		st := s.Inspect(metas[i])
		if st.State == Running {
			continue
		}
		kept++
		if kept <= keepFinished {
			continue
		}
		if st.State == Dead {
			_ = s.cleanUp(metas[i], &Report{})
		}
		_ = os.RemoveAll(s.dir(metas[i].ID))
	}
}

func (s *Store) readGroups(id string) []execrun.Process {
	raw, err := os.ReadFile(filepath.Join(s.dir(id), "groups"))
	if err != nil {
		return nil
	}
	var groups []execrun.Process
	for _, line := range strings.Split(string(raw), "\n") {
		var p execrun.Process
		if json.Unmarshal([]byte(line), &p) == nil && p.PID > 0 {
			groups = append(groups, p)
		}
	}
	return groups
}

// Recorder is the side of a detached run that executes the command: it records the process
// groups the run starts and the exit record it ends with.
type Recorder struct {
	dir    string
	result any
}

// OpenRecorder opens the run directory that the launching quai process created for id.
func OpenRecorder(commonDir, id string) (*Recorder, error) {
	if !runIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid %s %q", EnvRunID, id)
	}
	dir := NewStore(commonDir).dir(id)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("open detached run: %w", err)
	}
	return &Recorder{dir: dir}, nil
}

// ProcessStarted records a step's process group, so a wait or stop can kill it if the run dies.
func (r *Recorder) ProcessStarted(p execrun.Process) {
	line, err := json.Marshal(p)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "groups"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// SetResult keeps the structured result (a verify result) that Finish records.
func (r *Recorder) SetResult(result any) { r.result = result }

// Finish writes the run's exit record.
func (r *Recorder) Finish(code int) error {
	e := Exit{ExitCode: code, FinishedAt: time.Now().UTC()}
	if r.result != nil {
		raw, err := json.Marshal(r.result)
		if err != nil {
			return err
		}
		e.Result = raw
	}
	return writeJSONAtomic(filepath.Join(r.dir, "exit.json"), e)
}

func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
