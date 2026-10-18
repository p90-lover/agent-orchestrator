package processenv

import (
	"sort"
	"strings"
)

// MaxWindowsPATH caps the PATH handed to a Chat agent on Windows. Git Bash
// (MSYS2/Cygwin) converts PATH and the rest of the environment into its own
// form in every process it forks; an oversized PATH, grown by each launcher that
// prepends its own directory, is one of the things that makes that fork fail
// ("dofork: child -1 - CreateProcess failed ... 0xC0000142"). Entries past the
// cap are the lowest-priority ones and are dropped.
const MaxWindowsPATH = 16 * 1024

// windowsEssentials are variables a Windows process needs to start at all:
// without SystemRoot/windir system DLLs fail to initialise (STATUS_DLL_INIT_
// FAILED, 0xC0000142), without TEMP/TMP and USERPROFILE MSYS2 and Node cannot
// place their temporary and home files, and ComSpec/PATHEXT are how shells find
// cmd.exe and executables. An agent's Bash tool runs Git Bash, which needs all
// of them in every process it forks.
var windowsEssentials = []string{
	"SystemRoot", "windir", "SystemDrive", "ComSpec", "PATHEXT",
	"TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ProgramData",
}

const defaultPATHEXT = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"

// hardenWindows completes a Windows child environment (KEY=VALUE entries, keys
// compared case-insensitively): each essential variable that is missing or
// empty is filled from the others or from fallback (the system's own answer),
// and PATH is de-duplicated and capped at MaxWindowsPATH. Values the
// environment already holds are never replaced.
func hardenWindows(env []string, fallback func(string) string) []string {
	index := make(map[string]int, len(env))
	out := append([]string(nil), env...)
	for i, entry := range out {
		if key, _, ok := strings.Cut(entry, "="); ok && key != "" {
			index[strings.ToUpper(key)] = i
		}
	}
	get := func(name string) string {
		if i, ok := index[strings.ToUpper(name)]; ok {
			_, value, _ := strings.Cut(out[i], "=")
			return value
		}
		return ""
	}
	set := func(name, value string) {
		if value == "" {
			return
		}
		if i, ok := index[strings.ToUpper(name)]; ok {
			key, _, _ := strings.Cut(out[i], "=")
			out[i] = key + "=" + value
			return
		}
		index[strings.ToUpper(name)] = len(out)
		out = append(out, name+"="+value)
	}
	first := func(values ...string) string {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
		return ""
	}
	if fallback == nil {
		fallback = func(string) string { return "" }
	}
	for _, name := range windowsEssentials {
		if strings.TrimSpace(get(name)) != "" {
			continue
		}
		var value string
		switch strings.ToUpper(name) {
		case "SYSTEMROOT":
			value = first(get("windir"), fallback(name), `C:\Windows`)
		case "WINDIR":
			value = first(get("SystemRoot"), fallback(name))
		case "SYSTEMDRIVE":
			if root := get("SystemRoot"); len(root) >= 2 && root[1] == ':' {
				value = root[:2]
			}
			value = first(value, fallback(name))
		case "COMSPEC":
			if root := get("SystemRoot"); root != "" {
				value = strings.TrimRight(root, `\/`) + `\System32\cmd.exe`
			}
			value = first(value, fallback(name))
		case "PATHEXT":
			value = first(fallback(name), defaultPATHEXT)
		case "TEMP":
			value = first(get("TMP"), fallback(name))
		case "TMP":
			value = first(get("TEMP"), fallback(name))
		default:
			value = fallback(name)
		}
		set(name, value)
	}
	if i, ok := index["PATH"]; ok {
		key, value, _ := strings.Cut(out[i], "=")
		out[i] = key + "=" + compactPATH(value, MaxWindowsPATH)
	}
	sort.Strings(out)
	return out
}

// compactPATH drops empty and repeated entries (Windows paths compare without
// case or a trailing separator) and stops before the list would pass limit.
func compactPATH(value string, limit int) string {
	seen := make(map[string]struct{})
	kept := make([]string, 0, 32)
	size := 0
	for _, entry := range strings.Split(value, ";") {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(strings.TrimRight(strings.Trim(trimmed, `"`), `\/`))
		if _, dup := seen[key]; dup {
			continue
		}
		grow := len(trimmed)
		if len(kept) > 0 {
			grow++
		}
		if size+grow > limit && len(kept) > 0 {
			break
		}
		seen[key] = struct{}{}
		kept = append(kept, trimmed)
		size += grow
	}
	return strings.Join(kept, ";")
}
