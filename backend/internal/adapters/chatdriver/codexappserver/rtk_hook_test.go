package codexappserver

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAppServerArgsStayPlainWithoutRTKHook(t *testing.T) {
	for _, env := range [][]string{nil, {"PATH=/bin"}, {EnvRTKCodexHook + "="}, {EnvRTKCodexHook + "=   "}} {
		if got := appServerArgs(env); !slices.Equal(got, []string{"app-server"}) {
			t.Fatalf("appServerArgs(%q) = %q, want plain app-server", env, got)
		}
	}
	for _, bad := range []string{
		"rtk hook codex\nrm -rf /", "rtk\x00", "rtk\u2028hook codex", "rtk \xff", strings.Repeat("x", 1025),
	} {
		if got := appServerArgs([]string{EnvRTKCodexHook + "=" + bad}); !slices.Equal(got, []string{"app-server"}) {
			t.Fatalf("a malformed hook %q was registered: %q", bad, got)
		}
	}
}

func TestAppServerArgsRegisterAndTrustOnlyRTKHook(t *testing.T) {
	got := appServerArgs([]string{"PATH=/bin", EnvRTKCodexHook + "= rtk hook codex "})
	key := `"/<session-flags>/config.toml:pre_tool_use:0:0"`
	if runtime.GOOS == "windows" {
		key = `"C:\\<session-flags>\\config.toml:pre_tool_use:0:0"`
	}
	want := []string{
		"-c", `hooks.PreToolUse=[{matcher="^Bash$",hooks=[{type="command",command="rtk hook codex",timeout=10}]}]`,
		"-c", `hooks.state={` + key + `={trusted_hash="sha256:fec65c4110f3ab63f3b70ba26177340548945f2913c3933af88a63b94464fba9"}}`,
		"app-server",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("appServerArgs =\n%q\nwant\n%q", got, want)
	}
}

// Each -c value is TOML that Codex parses; a value that does not parse is taken
// as a plain string, which would break app-server's config instead of adding a
// hook. Quotes, backslashes and non-ASCII in the command must survive.
func TestAppServerArgsAreTOMLThatKeepsTheCommand(t *testing.T) {
	command := `& 'C:\Prögram Files\rtk.exe' hook codex <x> "y" && echo ok`
	args := appServerArgs([]string{EnvRTKCodexHook + "=" + command})
	if len(args) != 5 {
		t.Fatalf("args = %q", args)
	}
	var hooks struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct {
					Type    string
					Command string
					Timeout int
				}
			}
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
			}
		}
	}
	document := strings.Replace(args[1], "=", " = ", 1) + "\n" + strings.Replace(args[3], "=", " = ", 1) + "\n"
	if err := toml.Unmarshal([]byte(document), &hooks); err != nil {
		t.Fatalf("overrides are not TOML: %v\n%s", err, document)
	}
	pre := hooks.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "^Bash$" || len(pre[0].Hooks) != 1 ||
		pre[0].Hooks[0].Type != "command" || pre[0].Hooks[0].Command != command || pre[0].Hooks[0].Timeout != 10 {
		t.Fatalf("PreToolUse = %#v", pre)
	}
	trust, ok := hooks.Hooks.State[sessionFlagsHookKey()]
	if len(hooks.Hooks.State) != 1 || !ok || trust.TrustedHash != codexHookTrustHash(command, 10) {
		t.Fatalf("state = %#v", hooks.Hooks.State)
	}
}

// The hashes Codex itself reported (hooks/list currentHash, codex app-server
// 0.164.0) for session-flag hooks passed the way AO passes them. A hash that
// differs leaves the hook "modified" and never run.
func TestCodexHookTrustHashMatchesCodex(t *testing.T) {
	for _, tc := range []struct {
		command string
		timeout int
		want    string
	}{
		{"rtk hook codex", 10, "sha256:fec65c4110f3ab63f3b70ba26177340548945f2913c3933af88a63b94464fba9"},
		{`& 'C:\Prögram Files\rtk.exe' hook codex <x> "y" && echo ok`, 10,
			"sha256:675b29cc56fededb32b34368ef3654c3099498e95b4bc795b9b96c368f6b7d97"},
		{`"C:\Users\simon\AppData\Local\Microsoft\WinGet\Packages\rtk-ai.rtk_Microsoft.Winget.Source_8wekyb3d8bbwe\rtk.exe" hook codex`, 600,
			"sha256:b278dd3def64395592aec7411f120dccf09d5a80a464bb3d27bf648a8c65c72b"},
	} {
		if got := codexHookTrustHash(tc.command, tc.timeout); got != tc.want {
			t.Errorf("codexHookTrustHash(%q, %d) = %s, want %s", tc.command, tc.timeout, got, tc.want)
		}
	}
}

// The persistent chat host is how shipped chats start app-server: the hook rides
// its argv, from the session's environment over the daemon's, and a prepared
// launch environment decides for itself.
func TestPersistentHostLaunchCarriesRTKHook(t *testing.T) {
	hostArgv := func(t *testing.T, env map[string]string, prepare func(context.Context) (map[string]string, error)) (start, prepared []string) {
		t.Helper()
		d, _ := newTestDriver(t)
		d.persistent = true
		var cfg persistenthost.Config
		d.connectHost = func(_ context.Context, got persistenthost.Config) (*persistenthost.Transport, error) {
			cfg = got
			return nil, persistenthost.ErrAttached
		}
		_, err := d.Start(context.Background(), ports.ChatStartConfig{
			SessionID: "ao-rtk", DataDir: t.TempDir(), WorkspacePath: t.TempDir(), Env: env, PrepareEnv: prepare,
		})
		if !errors.Is(err, persistenthost.ErrAttached) {
			t.Fatalf("Start error = %v", err)
		}
		if cfg.Prepare != nil {
			launch, prepareErr := cfg.Prepare(context.Background())
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			prepared = launch.Argv
		}
		return cfg.Argv, prepared
	}
	registered := func(argv []string) bool {
		return len(argv) == 6 && argv[0] == "codex" && argv[1] == "-c" &&
			strings.Contains(argv[2], `command="rtk hook codex"`) && argv[5] == "app-server"
	}

	t.Setenv(EnvRTKCodexHook, "")
	if argv, _ := hostArgv(t, nil, nil); !slices.Equal(argv, []string{"codex", "app-server"}) {
		t.Fatalf("argv without RTK = %q", argv)
	}
	if argv, _ := hostArgv(t, map[string]string{EnvRTKCodexHook: "rtk hook codex"}, nil); !registered(argv) {
		t.Fatalf("session hook not registered: %q", argv)
	}
	t.Setenv(EnvRTKCodexHook, "rtk hook codex")
	if argv, _ := hostArgv(t, nil, nil); !registered(argv) {
		t.Fatalf("daemon hook not registered: %q", argv)
	}
	if argv, _ := hostArgv(t, map[string]string{EnvRTKCodexHook: ""}, nil); !slices.Equal(argv, []string{"codex", "app-server"}) {
		t.Fatalf("a session's empty hook must turn RTK off: %q", argv)
	}
	_, prepared := hostArgv(t, nil, func(context.Context) (map[string]string, error) {
		return map[string]string{EnvRTKCodexHook: ""}, nil
	})
	if !slices.Equal(prepared, []string{"codex", "app-server"}) {
		t.Fatalf("prepared launch kept a hook its environment turned off: %q", prepared)
	}
}
