package processenv

import (
	"strings"
	"testing"
)

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		out[strings.ToUpper(key)] = value
	}
	return out
}

// A daemon started with a stripped environment (no SystemRoot, windir, TEMP, ...)
// passed it on to the chat agent, and Git Bash's forked processes failed to
// initialise their DLLs (0xC0000142). Every essential variable is filled in.
func TestHardenWindowsFillsMissingEssentials(t *testing.T) {
	fallback := func(name string) string {
		switch strings.ToUpper(name) {
		case "SYSTEMROOT":
			return `D:\Win`
		case "USERPROFILE":
			return `C:\Users\me`
		case "LOCALAPPDATA":
			return `C:\Users\me\AppData\Local`
		}
		return ""
	}
	got := envMap(hardenWindows([]string{"Path=C:\\tools", "TMP=C:\\t"}, fallback))
	want := map[string]string{
		"SYSTEMROOT":   `D:\Win`,
		"WINDIR":       `D:\Win`,
		"SYSTEMDRIVE":  `D:`,
		"COMSPEC":      `D:\Win\System32\cmd.exe`,
		"PATHEXT":      defaultPATHEXT,
		"TEMP":         `C:\t`,
		"TMP":          `C:\t`,
		"USERPROFILE":  `C:\Users\me`,
		"LOCALAPPDATA": `C:\Users\me\AppData\Local`,
		"PATH":         `C:\tools`,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestHardenWindowsKeepsValuesAndSpelling(t *testing.T) {
	env := []string{"SystemRoot=C:\\Windows", "windir=C:\\Windows", "TEMP=C:\\Temp", "ComSpec=C:\\x\\cmd.exe"}
	out := hardenWindows(env, nil)
	for _, entry := range env {
		found := false
		for _, kept := range out {
			if kept == entry {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q was changed: %v", entry, out)
		}
	}
	if got := envMap(out)["TMP"]; got != `C:\Temp` {
		t.Fatalf("TMP = %q, want TEMP's value", got)
	}
}

func TestHardenWindowsCompactsPATH(t *testing.T) {
	out := envMap(hardenWindows([]string{`Path=C:\a;;c:\A\;C:\b;"C:\a";C:\c`}, nil))
	if got := out["PATH"]; got != `C:\a;C:\b;C:\c` {
		t.Fatalf("PATH = %q", got)
	}
	long := strings.Repeat(`C:\some\long\directory\name\`, 1)
	var parts []string
	for i := 0; i < 2000; i++ {
		parts = append(parts, long+strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
	capped := envMap(hardenWindows([]string{"PATH=" + strings.Join(parts, ";")}, nil))["PATH"]
	if len(capped) > MaxWindowsPATH {
		t.Fatalf("PATH is %d characters, over the %d cap", len(capped), MaxWindowsPATH)
	}
	if !strings.HasPrefix(capped, parts[0]+";"+parts[1]+";") {
		t.Fatal("the highest-priority PATH entries were not kept first")
	}
}
