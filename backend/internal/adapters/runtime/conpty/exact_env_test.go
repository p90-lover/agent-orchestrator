package conpty

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// An exact spawn gives the pty-host (and so the child it starts) the
// configured environment and the terminal's own TERM and COLORTERM, and
// nothing planted in the daemon's environment.
func TestHostEnvironmentExactDropsTheDaemonsEnvironment(t *testing.T) {
	t.Setenv("SSH_SK_PROVIDER", `C:\planted\sk.dll`)
	t.Setenv("SSH_AUTH_SOCK", `\\.\pipe\planted`)
	t.Setenv("AO_X", "1")
	configured := map[string]string{"SystemRoot": `C:\Windows`, "SSH_ASKPASS_REQUIRE": "never"}

	exact := hostEnvironment(withExactEnv(context.Background()), configured, nil)
	if got, want := envNames(exact), []string{"COLORTERM", "SSH_ASKPASS_REQUIRE", "SystemRoot", "TERM"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exact environment names = %v, want %v", got, want)
	}

	// Every other spawn is unchanged: the daemon's environment with additions.
	merged := strings.Join(hostEnvironment(context.Background(), configured, nil), "\n")
	for _, planted := range []string{"SSH_SK_PROVIDER=", "SSH_AUTH_SOCK=", "AO_X=1"} {
		if !strings.Contains(merged, planted) {
			t.Fatalf("merged environment lost %s", planted)
		}
	}
}

// Create asks the spawner for an exact environment only when the config does.
func TestCreatePassesExactEnvironmentToTheSpawner(t *testing.T) {
	for _, exact := range []bool{false, true} {
		isolateRegistry(t)
		var asked bool
		runtime := New(Options{Spawner: func(ctx context.Context, _, _ string, _ []string, _ map[string]string) (string, int, error) {
			asked = exactEnvRequested(ctx)
			return "", 0, errors.New("spawn refused in test")
		}})
		_, _ = runtime.Create(context.Background(), ports.RuntimeConfig{
			SessionID: "shellterm-exact-env", WorkspacePath: t.TempDir(), Argv: []string{"ssh"}, EnvExact: exact,
		})
		if asked != exact {
			t.Fatalf("EnvExact %v: spawner saw exact = %v", exact, asked)
		}
	}
}
