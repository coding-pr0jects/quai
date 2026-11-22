package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**", "a/b/c.go", true},
		{"**", "README.md", true},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"docs/**", "other/docs/a.md", false},
		{"**/*.test.ts", "foo.test.ts", true},
		{"**/*.test.ts", "apps/agent-host/test/foo.test.ts", true},
		{"**/*.test.ts", "apps/agent-host/test/foo.ts", false},
		{"apps/**/src/**", "apps/desktop/src/main.ts", true},
		{"apps/**/src/**", "apps/src/main.ts", true},
		{"apps/desktop/src/renderer/**", "apps/desktop/src/renderer/App.svelte", true},
		{"apps/desktop/src/renderer/**", "apps/desktop/src/main/index.ts", false},
		{"**/test/**", "apps/agent-host/test/a.ts", true},
		{"**/test/**", "test/a.ts", true},
		{"package.json", "package.json", true},
		{"package.json", "apps/desktop/package.json", false},
	}

	for _, tc := range cases {
		if got := Match(tc.pattern, tc.path); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestIncluded(t *testing.T) {
	cases := []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{"**"}, "main.go", true},
		{[]string{"**", "!docs/evidence/**"}, "docs/evidence/shot.png", false},
		{[]string{"**", "!docs/evidence/**"}, "docs/guide.md", true},
		{[]string{"!docs/evidence/**", "**"}, "docs/evidence/a/b.png", false},
		{[]string{"**/*.go", "!**/*_test.go"}, "internal/a/a_test.go", false},
		{[]string{"**/*.go", "!**/*_test.go"}, "internal/a/a.go", true},
		{[]string{"**/*.go", "!**/*_test.go"}, "README.md", false},
		{[]string{"!README.md"}, "main.go", false},
		{nil, "main.go", false},
	}
	for _, tc := range cases {
		if got := Included(tc.patterns, tc.path); got != tc.want {
			t.Errorf("Included(%q, %q) = %v, want %v", tc.patterns, tc.path, got, tc.want)
		}
	}
}
