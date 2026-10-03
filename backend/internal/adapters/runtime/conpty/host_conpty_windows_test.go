//go:build windows

package conpty

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConPTYChildExitClosesOutputAndRejectsResize(t *testing.T) {
	cmdPath := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	conn, err := newConPTY(t.TempDir(), cmdPath, []string{
		"/d", "/s", "/c", "echo ao-conpty-exit",
	})
	if err != nil {
		t.Fatalf("newConPTY: %v", err)
	}
	defer conn.Close()

	type readResult struct {
		output []byte
		err    error
	}
	resultC := make(chan readResult, 1)
	go func() {
		output, readErr := io.ReadAll(conn)
		resultC <- readResult{output: output, err: readErr}
	}()

	select {
	case result := <-resultC:
		if result.err != nil {
			t.Fatalf("read ConPTY output: %v", result.err)
		}
		if !bytes.Contains(result.output, []byte("ao-conpty-exit")) {
			t.Fatalf("output %q does not contain completion marker", result.output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ConPTY output did not close after the child exited")
	}
	if err := conn.Resize(120, 40); err == nil {
		t.Fatal("Resize succeeded after the pseudoconsole closed")
	}

	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("Done was not closed after the child exited")
	}
	if code, exited := conn.ExitCode(); !exited || code != 0 {
		t.Fatalf("ExitCode() = (%d, %v), want (0, true)", code, exited)
	}
}

// A batch launcher in a folder with spaces (like the managed
// "...\Coding Tools\tools\bin\agy.cmd") must start under ConPTY with every
// argument intact, including quoted ones.
func TestConPTYRunsABatchLauncherFromAPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Coding Tools", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "args.txt")
	launcher := filepath.Join(dir, "agent.cmd")
	script := "@echo off\r\n>\"" + marker + "\" echo ARGS=%*\r\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := newConPTY(t.TempDir(), launcher, []string{
		"--add-dir", `C:\Some Folder\work`, "--prompt-interactive", `Your task is in the file C:\x\launch-prompt.md - read it now.`,
	})
	if err != nil {
		t.Fatalf("newConPTY: %v", err)
	}
	defer conn.Close()
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	select {
	case <-conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the batch launcher did not finish")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the batch launcher never ran: %v", err)
	}
	want := `ARGS=--add-dir "C:\Some Folder\work" --prompt-interactive "Your task is in the file C:\x\launch-prompt.md - read it now."`
	if string(bytes.TrimSpace(got)) != want {
		t.Fatalf("launcher arguments\n got: %s\nwant: %s", bytes.TrimSpace(got), want)
	}
}
