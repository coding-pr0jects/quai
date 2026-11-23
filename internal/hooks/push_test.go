package hooks

import (
	"strings"
	"testing"

	"github.com/coding-pr0jects/quai/internal/config"
)

func TestParsePushRefs(t *testing.T) {
	zero := zeroSHA
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	cases := []struct {
		name    string
		input   string
		want    int
		wantErr string
	}{
		{
			name:  "single branch",
			input: "refs/heads/feat " + sha + " refs/heads/feat " + zero + "\n",
			want:  1,
		},
		{
			name:  "blank lines ignored",
			input: "\nrefs/heads/a " + sha + " refs/heads/a " + other + "\n\n",
			want:  1,
		},
		{
			name:    "three fields",
			input:   "refs/heads/feat " + sha + " refs/heads/feat\n",
			wantErr: "expected 4 fields, got 3",
		},
		{
			name:    "five fields",
			input:   "a b c d e\n",
			wantErr: "expected 4 fields, got 5",
		},
		{
			name:    "short local sha",
			input:   "refs/heads/feat abc refs/heads/feat " + zero + "\n",
			wantErr: "invalid SHA",
		},
		{
			name:    "non-hex remote sha",
			input:   "refs/heads/feat " + sha + " refs/heads/feat " + strings.Repeat("g", 40) + "\n",
			wantErr: "invalid SHA",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := ParsePushRefs(strings.NewReader(tc.input))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePushRefs: %v", err)
			}
			if len(refs) != tc.want {
				t.Fatalf("got %d refs, want %d", len(refs), tc.want)
			}
		})
	}
}

func TestBranchTargetsFiltersNonBranchUpdates(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	refs := []PushRef{
		{LocalRef: "refs/heads/feat", LocalSHA: sha, RemoteRef: "refs/heads/feat", RemoteSHA: zeroSHA},
		{LocalRef: "refs/heads/main", LocalSHA: other, RemoteRef: "refs/heads/main", RemoteSHA: sha},
		{LocalRef: "refs/heads/gone", LocalSHA: zeroSHA, RemoteRef: "refs/heads/gone", RemoteSHA: sha},
		{LocalRef: "refs/tags/v1", LocalSHA: sha, RemoteRef: "refs/tags/v1", RemoteSHA: zeroSHA},
		{LocalRef: "refs/quai/records/fast-x", LocalSHA: sha, RemoteRef: "refs/quai/records/fast-x", RemoteSHA: zeroSHA},
	}
	got := BranchTargets(refs)
	if len(got) != 2 {
		t.Fatalf("BranchTargets = %+v, want 2 branch updates", got)
	}
	if got[0].Ref != "refs/heads/feat" || got[0].Commit != sha {
		t.Fatalf("first target = %+v", got[0])
	}
	if got[1].Ref != "refs/heads/main" || got[1].Commit != other {
		t.Fatalf("second target = %+v", got[1])
	}
}

func TestIsQuaiRefPush(t *testing.T) {
	if IsQuaiRefPush(nil) {
		t.Fatal("empty refs must not count as a quai ref push")
	}
	if !IsQuaiRefPush([]PushRef{{RemoteRef: "refs/quai/records/a"}, {RemoteRef: "refs/quai/records/b"}}) {
		t.Fatal("all refs/quai/* refs must count as a quai ref push")
	}
	if !IsQuaiRefPush([]PushRef{{RemoteRef: "refs/quai/verdicts/abc"}}) {
		t.Fatal("a verdict push must count as a quai ref push")
	}
	if !IsQuaiRefPush([]PushRef{{RemoteRef: "refs/quai/records/a"}, {RemoteRef: "refs/quai/verdicts/abc"}}) {
		t.Fatal("a mixed records/verdicts push must count as a quai ref push")
	}
	if IsQuaiRefPush([]PushRef{{RemoteRef: "refs/quai/records/a"}, {RemoteRef: "refs/heads/main"}}) {
		t.Fatal("a branch ref must disqualify the quai-only shortcut")
	}
}

func TestDecideRequirement(t *testing.T) {
	missing := []CachedStep{{Step: "verify", Commit: strings.Repeat("a", 40), Ref: "refs/heads/feat"}}
	cases := []struct {
		name            string
		missing         []CachedStep
		onMissing       config.OnMissing
		allowUnverified bool
		unknownRef      bool
		blocked         bool
		bypassed        bool
		wantContains    []string
	}{
		{
			name: "satisfied",
		},
		{
			name:         "warn allows the push",
			missing:      missing,
			onMissing:    config.OnMissingWarn,
			wantContains: []string{`step "verify"`, "refs/heads/feat", "run quai verify first"},
		},
		{
			name:         "refuse blocks the push",
			missing:      missing,
			onMissing:    config.OnMissingRefuse,
			blocked:      true,
			wantContains: []string{`step "verify"`, "run quai verify first", `on_missing = "warn"`},
		},
		{
			name:            "escape hatch downgrades refuse",
			missing:         missing,
			onMissing:       config.OnMissingRefuse,
			allowUnverified: true,
			bypassed:        true,
			wantContains:    []string{EnvAllowUnverifiedPush, `step "verify"`},
		},
		{
			name:            "escape hatch leaves warn alone",
			missing:         missing,
			onMissing:       config.OnMissingWarn,
			allowUnverified: true,
			wantContains:    []string{"run quai verify first"},
		},
		{
			name:         "unknown ref is satisfied under warn",
			onMissing:    config.OnMissingWarn,
			unknownRef:   true,
			wantContains: []string{"pushed ref is unknown", "run quai verify first"},
		},
		{
			name:         "unknown ref refuses under refuse even when HEAD is cached",
			onMissing:    config.OnMissingRefuse,
			unknownRef:   true,
			blocked:      true,
			wantContains: []string{"pushed ref is unknown", "quai hooks install"},
		},
		{
			name:            "escape hatch covers an unknown ref",
			onMissing:       config.OnMissingRefuse,
			allowUnverified: true,
			unknownRef:      true,
			bypassed:        true,
			wantContains:    []string{"pushed ref is unknown", EnvAllowUnverifiedPush},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideRequirement(tc.missing, config.PrePushPolicy{OnMissing: tc.onMissing}, tc.allowUnverified, tc.unknownRef)
			if got.blocked != tc.blocked || got.bypassed != tc.bypassed {
				t.Fatalf("decision = %+v, want blocked=%v bypassed=%v", got, tc.blocked, tc.bypassed)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got.message, want) {
					t.Fatalf("message %q does not contain %q", got.message, want)
				}
			}
			if len(tc.missing) == 0 && !tc.unknownRef && got.message != "" {
				t.Fatalf("satisfied requirement produced message %q", got.message)
			}
		})
	}
}

func TestReadPushRefsEmptyPipeIsProvided(t *testing.T) {
	refs, provided, err := ReadPushRefs(strings.NewReader(""))
	if err != nil || !provided || len(refs) != 0 {
		t.Fatalf("empty pipe: refs=%v provided=%v err=%v, want provided with no refs", refs, provided, err)
	}
}
