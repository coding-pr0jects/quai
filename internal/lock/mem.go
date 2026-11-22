package lock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadAvailableMemory returns the host's currently available RAM in bytes on Linux
// by reading MemAvailable (or MemFree as fallback) from /proc/meminfo.
// Returns (0, false) on platforms where /proc/meminfo is unavailable.
func ReadAvailableMemory() (uint64, bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return ParseMeminfoAvailable(string(raw))
}

// ParseMeminfoAvailable extracts MemAvailable (or MemFree fallback) in bytes from /proc/meminfo text.
func ParseMeminfoAvailable(content string) (uint64, bool) {
	var memFree uint64
	hasFree := false
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "MemAvailable" && key != "MemFree" {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		bytes := kb * 1024
		if key == "MemAvailable" {
			return bytes, true
		}
		memFree = bytes
		hasFree = true
	}
	return memFree, hasFree
}

// FormatBytes renders a byte count in human-readable binary units.
func FormatBytes(b uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
