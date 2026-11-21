package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndValidation(t *testing.T) {
	raw := `
version = 1

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "go vet ./..."
hook = "pre-push"

[[steps]]
name = "test"
run = "go test ./... --since {{base}}"
inputs = ["**/*.go", "go.mod"]
lockfiles = ["go.sum"]
pool = "heavy"
slots = 1
timeout = "10m"
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Base != "main" {
		t.Fatalf("Base = %q, want main", cfg.Base)
	}
	if cfg.Tiers.Default != TierDiffAudit {
		t.Fatalf("Tiers.Default = %q, want diff-audit", cfg.Tiers.Default)
	}
	if !*cfg.Tiers.PureRenamesGateOnly || !*cfg.Tiers.EscalateOnTestTamper {
		t.Fatalf("tier booleans should default to true")
	}
	if cfg.Land.Method != MergeSquash || !*cfg.Land.OptimisticMerge {
		t.Fatalf("unexpected land defaults: %+v", cfg.Land)
	}
	if cfg.Land.LockWaitDur != 30*time.Minute || cfg.Land.ChecksWaitDur != 15*time.Minute {
		t.Fatalf("unexpected land durations: lock=%v checks=%v", cfg.Land.LockWaitDur, cfg.Land.ChecksWaitDur)
	}
	if len(cfg.Steps[0].Inputs) != 1 || cfg.Steps[0].Inputs[0] != "**" {
		t.Fatalf("step 0 inputs = %v, want [**]", cfg.Steps[0].Inputs)
	}
	if cfg.Steps[1].Duration != 10*time.Minute {
		t.Fatalf("step 1 duration = %v, want 10m", cfg.Steps[1].Duration)
	}
	if pre := cfg.PrePushSteps(); len(pre) != 1 || pre[0].Name != "fast" {
		t.Fatalf("PrePushSteps = %+v, want [fast]", pre)
	}
}

func TestParseRejectsInvalidConfigs(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{"wrong version", "version = 2\n", "unsupported version"},
		{"unknown key", "version = 1\nbogus = true\n", "unknown keys"},
		{"zero slot pool", "version = 1\n[slots]\nheavy = 0\n", "must be > 0"},
		{"duplicate step", "version = 1\n[[steps]]\nname = 'a'\nrun = 'true'\n[[steps]]\nname = 'a'\nrun = 'true'\n", "duplicate step"},
		{"unknown pool", "version = 1\n[[steps]]\nname = 'a'\nrun = 'true'\npool = 'nope'\nslots = 1\n", "unknown slot pool"},
		{"slots exceed cap", "version = 1\n[slots]\nheavy = 1\n[[steps]]\nname = 'a'\nrun = 'true'\npool = 'heavy'\nslots = 2\n", "capacity 1"},
		{"bad hook", "version = 1\n[[steps]]\nname = 'a'\nrun = 'true'\nhook = 'post-merge'\n", "unsupported hook"},
		{"bad tier", "version = 1\n[tiers]\ndefault = 'extreme'\n", "invalid tiers.default"},
		{"bad merge method", "version = 1\n[land]\nmethod = 'fast-forward'\n", "invalid land.method"},
		{"bad attest max_age", "version = 1\n[attest]\nmax_age = '0d'\n", "invalid attest.max_age"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.toml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestExpandCommandValidatesSHAs(t *testing.T) {
	sha := strings.Repeat("a", 40)
	got, err := ExpandCommand("check --since {{base}} {{head}} {{tree}}", sha, sha, sha)
	if err != nil {
		t.Fatalf("ExpandCommand failed: %v", err)
	}
	want := "check --since " + sha + " " + sha + " " + sha
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := ExpandCommand("check {{base}}", "main; rm -rf /", sha, sha); err == nil {
		t.Fatal("expected error for non-SHA placeholder value")
	}
}

func TestParseAttestPolicy(t *testing.T) {
	raw := `
version = 1

[[steps]]
name = "fast"
run = "go vet ./..."
signers = ["alice@example.com"]

[[steps]]
name = "test"
run = "go test ./..."

[attest]
allowed_signers = ".quai/allowed_signers"
signing_key = "~/.ssh/id_ed25519"
signers = ["ci@example.com"]
max_age = "7d"
remote = "upstream"
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Attest.AllowedSigners != ".quai/allowed_signers" {
		t.Fatalf("AllowedSigners = %q", cfg.Attest.AllowedSigners)
	}
	if cfg.Attest.MaxAgeDur != 7*24*time.Hour {
		t.Fatalf("MaxAgeDur = %v, want 168h", cfg.Attest.MaxAgeDur)
	}
	if cfg.Attest.Remote != "upstream" {
		t.Fatalf("Remote = %q, want upstream", cfg.Attest.Remote)
	}
	if got := cfg.AllowedPrincipalsFor(cfg.Steps[0]); len(got) != 1 || got[0] != "alice@example.com" {
		t.Fatalf("step 0 principals = %v", got)
	}
	if got := cfg.AllowedPrincipalsFor(cfg.Steps[1]); len(got) != 1 || got[0] != "ci@example.com" {
		t.Fatalf("step 1 principals = %v", got)
	}
}
