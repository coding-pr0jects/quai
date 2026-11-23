package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/coding-pr0jects/quai/internal/glob"
)

const FileName = ".quai.toml"

// MinAttestMaxAge is the shortest accepted [attest].max_age or --max-age.
const MinAttestMaxAge = time.Minute

type Tier string

const (
	TierGateOnly  Tier = "gate-only"
	TierDiffAudit Tier = "diff-audit"
	TierFullLive  Tier = "full-live"
)

type MergeMethod string

const (
	MergeSquash MergeMethod = "squash"
	MergeMerge  MergeMethod = "merge"
	MergeRebase MergeMethod = "rebase"
)

type SlotPool struct {
	Capacity         int
	MinFreeMem       string
	MinFreeMemBytes  uint64
	MinFreeDisk      string
	MinFreeDiskBytes uint64
}

// UnmarshalTOML accepts either an integer capacity (heavy = 2) or a table
// ([slots.heavy] capacity = 2, min_free_mem = "4G", min_free_disk = "2G").
func (p *SlotPool) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case int64:
		p.Capacity = int(val)
		return nil
	case map[string]any:
		for k, raw := range val {
			switch k {
			case "capacity":
				n, ok := raw.(int64)
				if !ok {
					return fmt.Errorf("slot pool capacity must be an integer")
				}
				p.Capacity = int(n)
			case "min_free_mem":
				s, ok := raw.(string)
				if !ok {
					return fmt.Errorf("slot pool min_free_mem must be a string (e.g. \"4G\")")
				}
				p.MinFreeMem = s
			case "min_free_disk":
				s, ok := raw.(string)
				if !ok {
					return fmt.Errorf("slot pool min_free_disk must be a string (e.g. \"2G\")")
				}
				p.MinFreeDisk = s
			default:
				return fmt.Errorf("unknown key %q in slot pool", k)
			}
		}
		return nil
	default:
		return fmt.Errorf("slot pool must be an integer or table with capacity/min_free_mem/min_free_disk")
	}
}

type Config struct {
	Version int                 `toml:"version"`
	Base    string              `toml:"base"`
	Slots   map[string]SlotPool `toml:"slots"`
	Steps   []Step              `toml:"steps"`
	Hooks   HooksPolicy         `toml:"hooks"`
	Tiers   TierPolicy          `toml:"tiers"`
	Land    LandPolicy          `toml:"land"`
	Attest  AttestPolicy        `toml:"attest"`
}

type Step struct {
	Name      string        `toml:"name"`
	Run       string        `toml:"run"`
	Inputs    []string      `toml:"inputs"`
	Lockfiles []string      `toml:"lockfiles"`
	DependsOn []string      `toml:"depends_on"`
	Hook      string        `toml:"hook"`
	Pool      string        `toml:"pool"`
	Slots     int           `toml:"slots"`
	PoolWhen  []string      `toml:"pool_when"`
	PoolTiers []Tier        `toml:"pool_tiers"`
	Timeout   string        `toml:"timeout"`
	Signers   []string      `toml:"signers"`
	Duration  time.Duration `toml:"-"`
}

// PoolRouting is the decision to acquire a step's pool or run it unlocked.
type PoolRouting struct {
	Acquire      bool
	MatchedPaths int
}

// HasConditionalPool reports whether a step decides its pool from the diff.
func (s Step) HasConditionalPool() bool {
	return len(s.PoolWhen) > 0 || len(s.PoolTiers) > 0
}

// RoutePool decides whether the step acquires its pool for this diff.
// Steps without pool_when or pool_tiers always acquire.
func (s Step) RoutePool(changedPaths []string, branchTier Tier) PoolRouting {
	if !s.HasConditionalPool() {
		return PoolRouting{Acquire: true}
	}
	matched := 0
	for _, p := range changedPaths {
		if glob.MatchAny(s.PoolWhen, p) {
			matched++
		}
	}
	if matched > 0 {
		return PoolRouting{Acquire: true, MatchedPaths: matched}
	}
	if len(s.PoolTiers) > 0 && branchTier.Rank() >= s.lowestPoolTier() {
		return PoolRouting{Acquire: true}
	}
	return PoolRouting{Acquire: false}
}

func (s Step) lowestPoolTier() int {
	lowest := s.PoolTiers[0].Rank()
	for _, t := range s.PoolTiers[1:] {
		if t.Rank() < lowest {
			lowest = t.Rank()
		}
	}
	return lowest
}

// OnMissing decides what the pre-push hook does when a required cache entry is absent.
type OnMissing string

const (
	OnMissingWarn   OnMissing = "warn"
	OnMissingRefuse OnMissing = "refuse"
)

type HooksPolicy struct {
	PrePush PrePushPolicy `toml:"pre_push"`
}

type PrePushPolicy struct {
	RequireCached []string  `toml:"require_cached"`
	OnMissing     OnMissing `toml:"on_missing"`
}

type AttestPolicy struct {
	AllowedSigners   string        `toml:"allowed_signers"`
	SigningKey       string        `toml:"signing_key"`
	Signers          []string      `toml:"signers"`
	MaxAge           string        `toml:"max_age"`
	Remote           string        `toml:"remote"`
	AutoSign         bool          `toml:"auto_sign"`
	AutoPush         bool          `toml:"auto_push"`
	AutoPrune        bool          `toml:"auto_prune"`
	PruneInterval    string        `toml:"prune_interval"`
	MaxAgeDur        time.Duration `toml:"-"`
	PruneIntervalDur time.Duration `toml:"-"`
}

type TierPolicy struct {
	Default              Tier     `toml:"default"`
	FullLive             []string `toml:"full_live"`
	DiffAudit            []string `toml:"diff_audit"`
	GateOnly             []string `toml:"gate_only"`
	PureRenamesGateOnly  *bool    `toml:"pure_renames_gate_only"`
	TestGlobs            []string `toml:"test_globs"`
	EscalateOnTestTamper *bool    `toml:"escalate_on_test_tamper"`
}

type LandPolicy struct {
	Method               MergeMethod   `toml:"method"`
	OptimisticMerge      *bool         `toml:"optimistic_merge"`
	RequiredChecks       []string      `toml:"required_checks"`
	LockWait             string        `toml:"lock_wait"`
	ChecksWait           string        `toml:"checks_wait"`
	GlobalPaths          []string      `toml:"global_paths"`
	RequireVerdictTiers  []Tier        `toml:"require_verdict_tiers"`
	RequireSignedVerdict bool          `toml:"require_signed_verdict"`
	AutoRestack          bool          `toml:"auto_restack"`
	LockWaitDur          time.Duration `toml:"-"`
	ChecksWaitDur        time.Duration `toml:"-"`
}

var (
	stepNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)
	sha40Re    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func boolPtr(v bool) *bool { return &v }

// Load reads and validates .quai.toml from repoRoot.
func Load(repoRoot string) (*Config, error) {
	p := filepath.Join(repoRoot, FileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("missing %s at %s: %w", FileName, p, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return Parse(data)
}

// Parse decodes and validates TOML bytes into a normalized Config.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		var keys []string
		for _, k := range undecoded {
			// BurntSushi/toml records subkeys decoded manually by SlotPool.UnmarshalTOML as undecoded.
			if len(k) == 3 && k[0] == "slots" && (k[2] == "capacity" || k[2] == "min_free_mem" || k[2] == "min_free_disk") {
				continue
			}
			keys = append(keys, k.String())
		}
		if len(keys) > 0 {
			return nil, fmt.Errorf("unknown keys in %s: %s", FileName, strings.Join(keys, ", "))
		}
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalizeAndValidate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported version %d in %s: expected 1", c.Version, FileName)
	}
	if c.Base == "" {
		c.Base = "main"
	}
	if strings.HasPrefix(c.Base, "-") {
		return fmt.Errorf("invalid base %q in %s: a base branch must not start with \"-\"; set base = \"main\" or the branch you target", c.Base, FileName)
	}
	for pool, sp := range c.Slots {
		if !stepNameRe.MatchString(pool) {
			return fmt.Errorf("invalid slot pool name %q: must match %s", pool, stepNameRe.String())
		}
		if sp.Capacity <= 0 {
			return fmt.Errorf("slot pool %q capacity must be > 0, got %d", pool, sp.Capacity)
		}
		if strings.TrimSpace(sp.MinFreeMem) != "" {
			b, err := ParseByteSize(sp.MinFreeMem)
			if err != nil || b == 0 {
				return fmt.Errorf("slot pool %q has invalid min_free_mem %q", pool, sp.MinFreeMem)
			}
			sp.MinFreeMemBytes = b
		}
		if strings.TrimSpace(sp.MinFreeDisk) != "" {
			b, err := ParseByteSize(sp.MinFreeDisk)
			if err != nil || b == 0 {
				return fmt.Errorf("slot pool %q has invalid min_free_disk %q", pool, sp.MinFreeDisk)
			}
			sp.MinFreeDiskBytes = b
		}
		c.Slots[pool] = sp
	}

	seen := make(map[string]struct{}, len(c.Steps))
	for i := range c.Steps {
		s := &c.Steps[i]
		if !stepNameRe.MatchString(s.Name) {
			return fmt.Errorf("invalid step name %q: must match %s", s.Name, stepNameRe.String())
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("duplicate step name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
		if strings.TrimSpace(s.Run) == "" {
			return fmt.Errorf("step %q has empty run command", s.Name)
		}
		if len(s.Inputs) == 0 {
			s.Inputs = []string{"**"}
		}
		if err := validateInputs(s); err != nil {
			return err
		}
		if s.Hook != "" && s.Hook != "pre-push" {
			return fmt.Errorf("step %q has unsupported hook %q: expected \"pre-push\" or empty", s.Name, s.Hook)
		}
		if s.HasConditionalPool() {
			if s.Pool == "" {
				return fmt.Errorf("step %q sets pool_when or pool_tiers without pool: set pool to a name from [slots]", s.Name)
			}
			if s.Slots == 0 {
				return fmt.Errorf("step %q sets pool_when or pool_tiers with slots = 0: conditional routing needs slots > 0", s.Name)
			}
		}
		for _, pattern := range s.PoolWhen {
			if strings.TrimSpace(pattern) == "" {
				return fmt.Errorf("step %q has an empty pool_when glob", s.Name)
			}
		}
		for _, t := range s.PoolTiers {
			if !ValidTier(t) {
				return fmt.Errorf("invalid tier %q in step %q pool_tiers", t, s.Name)
			}
		}
		if s.Slots < 0 {
			return fmt.Errorf("step %q slots must be >= 0, got %d", s.Name, s.Slots)
		}
		if s.Slots > 0 {
			if s.Pool == "" {
				return fmt.Errorf("step %q requests %d slot(s) without a pool", s.Name, s.Slots)
			}
			sp, ok := c.Slots[s.Pool]
			if !ok {
				return fmt.Errorf("step %q references unknown slot pool %q", s.Name, s.Pool)
			}
			if s.Slots > sp.Capacity {
				return fmt.Errorf("step %q requests %d slots in pool %q (capacity %d)", s.Name, s.Slots, s.Pool, sp.Capacity)
			}
		} else if s.Pool != "" {
			if _, ok := c.Slots[s.Pool]; !ok {
				return fmt.Errorf("step %q references unknown slot pool %q", s.Name, s.Pool)
			}
		}
		if s.Timeout == "" {
			s.Duration = 30 * time.Minute
		} else {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("step %q has invalid timeout %q", s.Name, s.Timeout)
			}
			s.Duration = d
		}
	}

	if err := c.validateDependencies(); err != nil {
		return err
	}

	if c.Hooks.PrePush.OnMissing == "" {
		c.Hooks.PrePush.OnMissing = OnMissingWarn
	}
	if !ValidOnMissing(c.Hooks.PrePush.OnMissing) {
		return fmt.Errorf("invalid hooks.pre_push.on_missing %q: expected \"warn\" or \"refuse\"", c.Hooks.PrePush.OnMissing)
	}
	required := make(map[string]struct{}, len(c.Hooks.PrePush.RequireCached))
	for _, name := range c.Hooks.PrePush.RequireCached {
		if _, ok := c.StepByName(name); !ok {
			return fmt.Errorf("hooks.pre_push.require_cached references unknown step %q", name)
		}
		if _, dup := required[name]; dup {
			return fmt.Errorf("hooks.pre_push.require_cached lists step %q twice", name)
		}
		required[name] = struct{}{}
	}

	if c.Tiers.Default == "" {
		c.Tiers.Default = TierDiffAudit
	}
	if !ValidTier(c.Tiers.Default) {
		return fmt.Errorf("invalid tiers.default %q", c.Tiers.Default)
	}
	if c.Tiers.PureRenamesGateOnly == nil {
		c.Tiers.PureRenamesGateOnly = boolPtr(true)
	}
	if c.Tiers.EscalateOnTestTamper == nil {
		c.Tiers.EscalateOnTestTamper = boolPtr(true)
	}

	if c.Land.Method == "" {
		c.Land.Method = MergeSquash
	}
	if !ValidMergeMethod(c.Land.Method) {
		return fmt.Errorf("invalid land.method %q", c.Land.Method)
	}
	if c.Land.OptimisticMerge == nil {
		c.Land.OptimisticMerge = boolPtr(true)
	}
	if c.Land.LockWait == "" {
		c.Land.LockWaitDur = 30 * time.Minute
	} else {
		d, err := time.ParseDuration(c.Land.LockWait)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid land.lock_wait %q", c.Land.LockWait)
		}
		c.Land.LockWaitDur = d
	}
	if c.Land.ChecksWait == "" {
		c.Land.ChecksWaitDur = 15 * time.Minute
	} else {
		d, err := time.ParseDuration(c.Land.ChecksWait)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid land.checks_wait %q", c.Land.ChecksWait)
		}
		c.Land.ChecksWaitDur = d
	}
	for _, vt := range c.Land.RequireVerdictTiers {
		if !ValidTier(vt) {
			return fmt.Errorf("invalid tier %q in land.require_verdict_tiers", vt)
		}
	}

	if c.Attest.Remote == "" {
		c.Attest.Remote = "origin"
	}
	if strings.HasPrefix(c.Attest.Remote, "-") {
		return fmt.Errorf("invalid [attest].remote %q in %s: a remote name must not start with \"-\"; set remote = \"origin\" or another configured git remote", c.Attest.Remote, FileName)
	}
	if c.Attest.AutoPush {
		c.Attest.AutoSign = true
	}
	if c.Attest.MaxAge == "" {
		c.Attest.MaxAgeDur = 14 * 24 * time.Hour
	} else {
		d, err := ParseDuration(c.Attest.MaxAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid attest.max_age %q", c.Attest.MaxAge)
		}
		if d < MinAttestMaxAge {
			return fmt.Errorf("invalid attest.max_age %q: must be at least %s", c.Attest.MaxAge, MinAttestMaxAge)
		}
		c.Attest.MaxAgeDur = d
	}
	if c.Attest.PruneInterval == "" {
		c.Attest.PruneIntervalDur = 24 * time.Hour
	} else {
		d, err := ParseDuration(c.Attest.PruneInterval)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid attest.prune_interval %q", c.Attest.PruneInterval)
		}
		c.Attest.PruneIntervalDur = d
	}
	return nil
}

func validateInputs(s *Step) error {
	positive := false
	for _, p := range s.Inputs {
		pattern, negated := strings.CutPrefix(p, "!")
		if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "!") {
			return fmt.Errorf("step %q has invalid inputs pattern %q", s.Name, p)
		}
		positive = positive || !negated
	}
	if !positive {
		return fmt.Errorf("step %q inputs only exclude paths: add a pattern to include, such as \"**\"", s.Name)
	}
	for _, p := range s.Lockfiles {
		if strings.HasPrefix(p, "!") {
			return fmt.Errorf("step %q lockfiles pattern %q is negated: lockfiles take no exclusions, exclude the path in inputs instead", s.Name, p)
		}
	}
	return nil
}

func (c *Config) validateDependencies() error {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(c.Steps))
	var visit func(s Step, chain []string) error
	visit = func(s Step, chain []string) error {
		switch state[s.Name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("steps depend on each other in a cycle: %s", strings.Join(append(chain, s.Name), " -> "))
		}
		state[s.Name] = visiting
		seen := make(map[string]struct{}, len(s.DependsOn))
		for _, dep := range s.DependsOn {
			if _, dup := seen[dep]; dup {
				return fmt.Errorf("step %q lists %q twice in depends_on", s.Name, dep)
			}
			seen[dep] = struct{}{}
			depStep, ok := c.StepByName(dep)
			if !ok {
				return fmt.Errorf("step %q depends on unknown step %q", s.Name, dep)
			}
			if err := visit(depStep, append(chain, s.Name)); err != nil {
				return err
			}
		}
		state[s.Name] = done
		return nil
	}
	for _, s := range c.Steps {
		if err := visit(s, nil); err != nil {
			return err
		}
	}
	return nil
}

// StepPlan returns the named steps, or every step when names is empty, each preceded by
// its transitive depends_on prerequisites. A step appears once, at its first place.
func (c *Config) StepPlan(names []string) ([]Step, error) {
	if len(names) == 0 {
		for _, s := range c.Steps {
			names = append(names, s.Name)
		}
	}
	var plan []Step
	placed := make(map[string]bool, len(c.Steps))
	var place func(name string) error
	place = func(name string) error {
		if placed[name] {
			return nil
		}
		step, ok := c.StepByName(name)
		if !ok {
			return fmt.Errorf("unknown step %q", name)
		}
		placed[name] = true
		for _, dep := range step.DependsOn {
			if err := place(dep); err != nil {
				return err
			}
		}
		plan = append(plan, step)
		return nil
	}
	for _, n := range names {
		if err := place(n); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// ParseByteSize parses human-readable memory sizes like "512M", "4G", "4GiB", or "2048MB".
func ParseByteSize(raw string) (uint64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	upper := strings.ToUpper(s)
	suffixes := []struct {
		suffix string
		mult   uint64
	}{
		{"GIB", 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
		{"MIB", 1024 * 1024},
		{"MB", 1024 * 1024},
		{"M", 1024 * 1024},
		{"KIB", 1024},
		{"KB", 1024},
		{"K", 1024},
		{"B", 1},
	}
	mult := uint64(1)
	numPart := upper
	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.suffix) {
			mult = sf.mult
			numPart = strings.TrimSpace(strings.TrimSuffix(upper, sf.suffix))
			break
		}
	}
	var val uint64
	if _, err := fmt.Sscanf(numPart, "%d", &val); err != nil || fmt.Sprintf("%d", val) != numPart || val == 0 {
		return 0, fmt.Errorf("invalid byte size %q", raw)
	}
	return val * mult, nil
}

// ParseDuration parses a Go duration string and additionally supports day suffixes like "14d".
func ParseDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if strings.HasSuffix(s, "d") && len(s) > 1 {
		daysStr := strings.TrimSuffix(s, "d")
		var days int
		if _, err := fmt.Sscanf(daysStr, "%d", &days); err == nil && fmt.Sprintf("%d", days) == daysStr && days > 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
		return 0, fmt.Errorf("invalid day duration %q", raw)
	}
	return time.ParseDuration(s)
}

// AllowedPrincipalsFor returns the signer principal allowlist for a step.
func (c *Config) AllowedPrincipalsFor(step Step) []string {
	if len(step.Signers) > 0 {
		return step.Signers
	}
	return c.Attest.Signers
}

func ValidTier(t Tier) bool {
	return t == TierGateOnly || t == TierDiffAudit || t == TierFullLive
}

// Rank orders tiers from gate-only (1) to full-live (3).
func (t Tier) Rank() int {
	switch t {
	case TierFullLive:
		return 3
	case TierDiffAudit:
		return 2
	default:
		return 1
	}
}

func ValidMergeMethod(m MergeMethod) bool {
	return m == MergeSquash || m == MergeMerge || m == MergeRebase
}

func ValidOnMissing(m OnMissing) bool {
	return m == OnMissingWarn || m == OnMissingRefuse
}

// StepByName returns the step matching name, or false if absent.
func (c *Config) StepByName(name string) (Step, bool) {
	for _, s := range c.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// PrePushSteps returns steps configured with hook = "pre-push".
func (c *Config) PrePushSteps() []Step {
	var out []Step
	for _, s := range c.Steps {
		if s.Hook == "pre-push" {
			out = append(out, s)
		}
	}
	return out
}

// RequireCachedSteps returns the steps named by [hooks.pre_push].require_cached, in config order.
func (c *Config) RequireCachedSteps() []Step {
	out := make([]Step, 0, len(c.Hooks.PrePush.RequireCached))
	for _, name := range c.Hooks.PrePush.RequireCached {
		if s, ok := c.StepByName(name); ok {
			out = append(out, s)
		}
	}
	return out
}

// ExpandCommand replaces {{base}}, {{head}}, and {{tree}} with validated 40-char hex git SHAs.
func ExpandCommand(template, baseSHA, headSHA, treeSHA string) (string, error) {
	replacements := [][2]string{
		{"{{base}}", baseSHA},
		{"{{head}}", headSHA},
		{"{{tree}}", treeSHA},
	}
	out := template
	for _, pair := range replacements {
		placeholder, sha := pair[0], pair[1]
		if !strings.Contains(out, placeholder) {
			continue
		}
		if !sha40Re.MatchString(sha) {
			return "", fmt.Errorf("invalid git SHA %q for placeholder %s", sha, placeholder)
		}
		out = strings.ReplaceAll(out, placeholder, sha)
	}
	return out, nil
}
