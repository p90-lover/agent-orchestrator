// Package processenv builds child-process environments shared by Chat drivers.
package processenv

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// Merge overlays session-specific values on the daemon environment and returns
// the KEY=VALUE form expected by os/exec. Sorting makes launches deterministic
// enough to inspect and compare in tests and process diagnostics.
//
// On Windows the result is also completed with the variables every process
// needs to start (SystemRoot, windir, TEMP, ...; see hardenWindows) and its
// PATH is de-duplicated and capped: an agent's Bash tool runs Git Bash, whose
// forked processes otherwise fail with STATUS_DLL_INIT_FAILED (0xC0000142).
func Merge(overlay map[string]string) []string {
	if runtime.GOOS == "windows" {
		return hardenWindows(merge(os.Environ(), overlay, true), systemDefault)
	}
	return merge(os.Environ(), overlay, false)
}

func merge(environ []string, overlay map[string]string, caseInsensitive bool) []string {
	merged := make(map[string]string, len(environ)+len(overlay))
	for _, entry := range environ {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if caseInsensitive {
				key = strings.ToUpper(key)
			}
			merged[key] = entry
		}
	}
	keys := make([]string, 0, len(overlay))
	for key := range overlay {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if caseInsensitive && strings.EqualFold(keys[i], "PATH") != strings.EqualFold(keys[j], "PATH") {
			return !strings.EqualFold(keys[i], "PATH")
		}
		if caseInsensitive && (keys[i] == "PATH") != (keys[j] == "PATH") {
			return keys[j] == "PATH"
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		value := overlay[key]
		entry := key + "=" + value
		if caseInsensitive {
			key = strings.ToUpper(key)
		}
		merged[key] = entry
	}

	out := make([]string, 0, len(merged))
	for _, entry := range merged {
		out = append(out, entry)
	}
	sort.Strings(out)
	return out
}
