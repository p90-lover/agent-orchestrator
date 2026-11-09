package opencodeacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// rtkPluginFile writes a plugin file under a folder whose name has a space, as
// Coding Tools' own data folder ("Coding Tools") does.
func rtkPluginFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Coding Tools", "rtk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "rtk.ts")
	if err := os.WriteFile(file, []byte("export default {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestFileURLIsWhatOpenCodeImports(t *testing.T) {
	var cases map[string]string
	if runtime.GOOS == "windows" {
		cases = map[string]string{
			`G:\AppData\Coding Tools\rtk\opencode\rtk.ts`: "file:///G:/AppData/Coding%20Tools/rtk/opencode/rtk.ts",
			`C:\a#b\100%\rtk.ts`:                          "file:///C:/a%23b/100%25/rtk.ts",
		}
	} else {
		cases = map[string]string{
			"/home/me/Coding Tools/rtk.ts": "file:///home/me/Coding%20Tools/rtk.ts",
			"/a#b/100%/rtk.ts":             "file:///a%23b/100%25/rtk.ts",
		}
	}
	for path, want := range cases {
		if got := fileURL(path); got != want {
			t.Errorf("fileURL(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRTKOpenCodePluginReadsTheSessionThenTheDaemon(t *testing.T) {
	session, daemon := rtkPluginFile(t), rtkPluginFile(t)
	t.Setenv(EnvRTKOpenCodePlugin, daemon)

	if got := rtkOpenCodePlugin(map[string]string{EnvRTKOpenCodePlugin: session}); got != fileURL(session) {
		t.Fatalf("session value: got %q, want %q", got, fileURL(session))
	}
	if got := rtkOpenCodePlugin(map[string]string{}); got != fileURL(daemon) {
		t.Fatalf("no session value: got %q, want the daemon's %q", got, fileURL(daemon))
	}
	if got := rtkOpenCodePlugin(map[string]string{EnvRTKOpenCodePlugin: ""}); got != "" {
		t.Fatalf("a session's empty value turns RTK off, got %q", got)
	}
}

func TestRTKOpenCodePluginNeedsAnAbsolutePathToAFile(t *testing.T) {
	t.Setenv(EnvRTKOpenCodePlugin, "")
	file := rtkPluginFile(t)
	for name, value := range map[string]string{
		"empty":     "  ",
		"relative":  filepath.Join("rtk", "rtk.ts"),
		"missing":   filepath.Join(filepath.Dir(file), "gone.ts"),
		"directory": filepath.Dir(file),
		"newline":   file + "\nfile:///elsewhere.ts",
		"too long":  file + strings.Repeat("x", 4096),
	} {
		if got := rtkOpenCodePlugin(map[string]string{EnvRTKOpenCodePlugin: value}); got != "" {
			t.Errorf("%s: got %q, want no plugin", name, got)
		}
	}
	if got := rtkOpenCodePlugin(map[string]string{EnvRTKOpenCodePlugin: "  " + file + "  "}); got != fileURL(file) {
		t.Errorf("surrounding spaces: got %q, want %q", got, fileURL(file))
	}
}

func TestWithRTKPluginAddsItAfterThePersonsPlugins(t *testing.T) {
	file := rtkPluginFile(t)
	env := map[string]string{EnvRTKOpenCodePlugin: file}
	content := `{"$schema":"https://opencode.ai/config.json","plugin":["opencode-wakatime","file:///mine.ts"],` +
		`"provider":{"openai":{"models":{"m":{"limit":{"context":1000000}}}}},"default_agent":"ao-default"}`

	got, err := withRTKPlugin(content, env)
	if err != nil {
		t.Fatalf("withRTKPlugin: %v", err)
	}
	var config struct {
		Plugin       []string `json:"plugin"`
		DefaultAgent string   `json:"default_agent"`
	}
	if err := json.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	want := []string{"opencode-wakatime", "file:///mine.ts", fileURL(file)}
	if strings.Join(config.Plugin, "|") != strings.Join(want, "|") {
		t.Fatalf("plugin = %q, want %q", config.Plugin, want)
	}
	if config.DefaultAgent != "ao-default" {
		t.Fatalf("the rest of the overlay changed: %s", got)
	}
	// Numbers keep the caller's spelling (a float64 round trip writes 1e+06).
	if !strings.Contains(got, `"context":1000000`) {
		t.Fatalf("limit rewritten: %s", got)
	}

	again, err := withRTKPlugin(got, env)
	if err != nil || again != got {
		t.Fatalf("a second pass must not list it twice: %s (%v)", again, err)
	}
}

func TestWithRTKPluginLeavesTheOverlayAloneWithoutIt(t *testing.T) {
	t.Setenv(EnvRTKOpenCodePlugin, "")
	content := `{"plugin":["file:///mine.ts"],"agent":{}}`
	if got, err := withRTKPlugin(content, map[string]string{}); err != nil || got != content {
		t.Fatalf("no RTK plugin: got %s (%v), want the overlay byte for byte", got, err)
	}
	// A "plugin" that is not a list is OpenCode's to report; RTK stays out.
	odd := `{"plugin":"file:///mine.ts"}`
	file := rtkPluginFile(t)
	if got, err := withRTKPlugin(odd, map[string]string{EnvRTKOpenCodePlugin: file}); err != nil || got != odd {
		t.Fatalf("non-list plugin: got %s (%v)", got, err)
	}
	if _, err := withRTKPlugin("{not json", map[string]string{EnvRTKOpenCodePlugin: file}); err == nil {
		t.Fatal("an overlay that is not JSON must fail the launch, as PrepareACPConfigContent does")
	}
}

func TestConfigureListsRTKsPluginWithAOsAgents(t *testing.T) {
	file := rtkPluginFile(t)
	_, env, err := configure(context.Background(), acpdriver.LaunchConfig{
		SessionID: "worker-1", Permissions: ports.PermissionModeAuto,
		Env: map[string]string{
			EnvRTKOpenCodePlugin:      file,
			"OPENCODE_CONFIG_CONTENT": `{"plugin":["file:///mine.ts"]}`,
		},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	var config struct {
		Plugin       []string                  `json:"plugin"`
		DefaultAgent string                    `json:"default_agent"`
		Agent        map[string]map[string]any `json:"agent"`
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if strings.Join(config.Plugin, "|") != "file:///mine.ts|"+fileURL(file) {
		t.Fatalf("plugin = %q", config.Plugin)
	}
	if config.DefaultAgent != "ao-auto" || len(config.Agent) != 4 {
		t.Fatalf("AO's agents lost: %#v", config)
	}

	t.Setenv(EnvRTKOpenCodePlugin, "")
	_, env, err = configure(context.Background(), acpdriver.LaunchConfig{SessionID: "worker-1"})
	if err != nil {
		t.Fatalf("configure without RTK: %v", err)
	}
	if strings.Contains(env["OPENCODE_CONFIG_CONTENT"], `"plugin"`) {
		t.Fatalf("RTK off still listed a plugin: %s", env["OPENCODE_CONFIG_CONTENT"])
	}
}
