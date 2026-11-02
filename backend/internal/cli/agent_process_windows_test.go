//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A batch launcher in a folder with spaces (like "...\Coding Tools\tools\bin\agy.cmd")
// must start and receive every argument, including quoted ones with spaces.
func TestSupervisedCommandRunsABatchLauncherFromAPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Coding Tools", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "agent.cmd")
	if err := os.WriteFile(launcher, []byte("@echo off\r\necho ARGS=%*\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := supervisedCommand(context.Background(), []string{
		launcher, "--add-dir", `C:\Some Folder\work`, "--prompt-interactive", "Your task is in the file C:\\x\\launch-prompt.md - read it now.",
	})
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("batch launcher did not run: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := `ARGS=--add-dir "C:\Some Folder\work" --prompt-interactive "Your task is in the file C:\x\launch-prompt.md - read it now."`
	if got != want {
		t.Fatalf("batch launcher output\n got: %s\nwant: %s", got, want)
	}
}

// Ordinary executables are started directly, without cmd.exe.
func TestSupervisedCommandStartsExecutablesDirectly(t *testing.T) {
	child := supervisedCommand(context.Background(), []string{`C:\tools\agent.exe`, "--flag"})
	if child.SysProcAttr != nil && child.SysProcAttr.CmdLine != "" {
		t.Fatalf("an .exe must not go through cmd.exe: %q", child.SysProcAttr.CmdLine)
	}
	if !strings.EqualFold(child.Path, `C:\tools\agent.exe`) {
		t.Fatalf("path = %q", child.Path)
	}
}
