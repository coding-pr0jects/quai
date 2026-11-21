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
)

const FileName = ".quai.toml"

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

type Config struct {
	Version int            `toml:"version"`
	Base    string         `toml:"base"`
	Slots   map[string]int `toml:"slots"`
	Steps   []Step         `toml:"steps"`
	Tiers   TierPolicy     `toml:"tiers"`
	Land    LandPolicy     `toml:"land"`
	Attest  AttestPolicy   `toml:"attest"`
}

type Step struct {
	Name      string        `toml:"name"`
	Run       string        `toml:"run"`
	Inputs    []string      `toml:"inputs"`
	Lockfiles []string      `toml:"lockfiles"`
	Hook      string        `toml:"hook"`
	Pool      string        `toml:"pool"`
	Slots     int           `toml:"slots"`
	Timeout   string        `toml:"timeout"`
	Signers   []string      `toml:"signers"`
	Duration  time.Duration `toml:"-"`
}

type AttestPolicy struct {
	AllowedSigners string        `toml:"allowed_signers"`
	SigningKey     string        `toml:"signing_key"`
	Signers        []string      `toml:"signers"`
	MaxAge         string        `toml:"max_age"`
	Remote         string        `toml:"remote"`
	MaxAgeDur      time.Duration `toml:"-"`
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
	Method          MergeMethod   `toml:"method"`
	OptimisticMerge *bool         `toml:"optimistic_merge"`
	RequiredChecks  []string      `toml:"required_checks"`
	LockWait        string        `toml:"lock_wait"`
	ChecksWait      string        `toml:"checks_wait"`
	GlobalPaths     []string      `toml:"global_paths"`
	LockWaitDur     time.Duration `toml:"-"`
	ChecksWaitDur   time.Duration `toml:"-"`
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
			return nil, fmt.Errorf("missing %s at %s", FileName, p)
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
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys in %s: %s", FileName, strings.Join(keys, ", "))
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
	for pool, count := range c.Slots {
		if !stepNameRe.MatchString(pool) {
			return fmt.Errorf("invalid slot pool name %q: must match %s", pool, stepNameRe.String())
		}
		if count <= 0 {
			return fmt.Errorf("slot pool %q must be > 0, got %d", pool, count)
		}
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
		if s.Hook != "" && s.Hook != "pre-push" {
			return fmt.Errorf("step %q has unsupported hook %q: expected \"pre-push\" or empty", s.Name, s.Hook)
		}
		if s.Slots < 0 {
			return fmt.Errorf("step %q slots must be >= 0, got %d", s.Name, s.Slots)
		}
		if s.Slots > 0 {
			if s.Pool == "" {
				return fmt.Errorf("step %q requests %d slot(s) without a pool", s.Name, s.Slots)
			}
			capSlots, ok := c.Slots[s.Pool]
			if !ok {
				return fmt.Errorf("step %q references unknown slot pool %q", s.Name, s.Pool)
			}
			if s.Slots > capSlots {
				return fmt.Errorf("step %q requests %d slots in pool %q (capacity %d)", s.Name, s.Slots, s.Pool, capSlots)
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

	if c.Attest.Remote == "" {
		c.Attest.Remote = "origin"
	}
	if c.Attest.MaxAge == "" {
		c.Attest.MaxAgeDur = 14 * 24 * time.Hour
	} else {
		d, err := ParseDuration(c.Attest.MaxAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid attest.max_age %q", c.Attest.MaxAge)
		}
		c.Attest.MaxAgeDur = d
	}
	return nil
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

func ValidMergeMethod(m MergeMethod) bool {
	return m == MergeSquash || m == MergeMerge || m == MergeRebase
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
