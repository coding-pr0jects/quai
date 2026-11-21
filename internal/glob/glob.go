package glob

import (
	"path"
	"strings"
)

// Match reports whether slash-separated filePath matches pattern.
// Supports *, ?, and ** across zero or more path segments.
func Match(pattern, filePath string) bool {
	pat := strings.TrimPrefix(path.Clean(strings.ReplaceAll(pattern, "\\", "/")), "./")
	fp := strings.TrimPrefix(path.Clean(strings.ReplaceAll(filePath, "\\", "/")), "./")
	if pat == "**" {
		return true
	}
	return matchSegments(strings.Split(pat, "/"), strings.Split(fp, "/"))
}

// MatchAny reports whether filePath matches any pattern in patterns.
func MatchAny(patterns []string, filePath string) bool {
	for _, p := range patterns {
		if Match(p, filePath) {
			return true
		}
	}
	return false
}

func matchSegments(patSegs, pathSegs []string) bool {
	pi, si := 0, 0
	for pi < len(patSegs) && si < len(pathSegs) {
		if patSegs[pi] == "**" {
			for pi+1 < len(patSegs) && patSegs[pi+1] == "**" {
				pi++
			}
			if pi == len(patSegs)-1 {
				return true
			}
			for k := si; k <= len(pathSegs); k++ {
				if matchSegments(patSegs[pi+1:], pathSegs[k:]) {
					return true
				}
			}
			return false
		}
		ok, err := path.Match(patSegs[pi], pathSegs[si])
		if err != nil || !ok {
			return false
		}
		pi++
		si++
	}
	for pi < len(patSegs) && patSegs[pi] == "**" {
		pi++
	}
	return pi == len(patSegs) && si == len(pathSegs)
}
