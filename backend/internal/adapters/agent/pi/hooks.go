package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const (
	piExtensionsDirName = "extensions"
	// AO's extension is a folder extension, .pi/extensions/ao/index.ts, whose own .gitignore
	// hides it from git. Pi's auto-discovery skips whatever a .gitignore in .pi/extensions itself
	// lists (it reads only that folder's ignore files, not a subfolder's), and a pi-acp chat has no
	// --extension flag, so the flat ao-activity.ts AO used to write, listed there, never loaded in
	// a chat (checked 2026-11-25 on pi-acp 0.17.1, Pi 0.75.5).
	piExtensionFolderName     = "ao"
	piExtensionFileName       = "index.ts"
	legacyPiExtensionFileName = "ao-activity.ts"
	piExtensionSentinel       = "agent-orchestrator: managed pi activity extension"
	minPiSettledVersion       = "0.80.6"
)

// PiGatewayProvider is the Pi provider the managed extension registers for a CPA gateway session.
const PiGatewayProvider = ports.PiGatewayProvider

var piVersionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

func piExtensionPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".pi", piExtensionsDirName, piExtensionFolderName, piExtensionFileName)
}

// GetAgentHooks installs AO's project-local Pi extension. Terminal launch and
// restore also pass it explicitly with --extension (Pi loads a path once
// however it was found); a pi-acp chat finds it by Pi's auto-discovery.
func (p *Plugin) GetAgentHooks(ctx context.Context, cfg ports.WorkspaceHookConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.WorkspacePath) == "" {
		return errors.New("pi.GetAgentHooks: WorkspacePath is required")
	}

	path := piExtensionPath(cfg.WorkspacePath)
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // caller-owned workspace path
		if !strings.Contains(string(data), piExtensionSentinel) {
			return fmt.Errorf("pi.GetAgentHooks: refusing to overwrite non-AO file at %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pi.GetAgentHooks: read managed extension: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("pi.GetAgentHooks: create extension dir: %w", err)
	}
	settledSupported, err := p.piAgentSettledSupported(ctx)
	if err != nil {
		return err
	}
	if err := hookutil.AtomicWriteFile(path, []byte(piActivityExtensionSource(settledSupported)), 0o600); err != nil {
		return fmt.Errorf("pi.GetAgentHooks: write extension: %w", err)
	}
	if err := hookutil.EnsureWorkspaceGitignore(filepath.Dir(path), piExtensionFileName); err != nil {
		return fmt.Errorf("pi.GetAgentHooks: gitignore: %w", err)
	}
	// The flat extension AO wrote before: the folder one replaces it. A file without AO's
	// sentinel is the user's and stays.
	legacy := filepath.Join(cfg.WorkspacePath, ".pi", piExtensionsDirName, legacyPiExtensionFileName)
	if data, err := os.ReadFile(legacy); err == nil && strings.Contains(string(data), piExtensionSentinel) { //nolint:gosec // caller-owned workspace path
		_ = os.Remove(legacy)
	}
	return nil
}

func appendPiExtensionFlag(cmd *[]string, workspacePath string) {
	if strings.TrimSpace(workspacePath) == "" {
		return
	}
	*cmd = append(*cmd, "--extension", piExtensionPath(workspacePath))
}

func (p *Plugin) piAgentSettledSupported(ctx context.Context) (bool, error) {
	binary, err := p.piBinary(ctx)
	if err != nil {
		return false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := aoprocess.CommandContext(probeCtx, binary, "--version").CombinedOutput() //nolint:gosec // binary is adapter-resolved, args are static.
	if err != nil {
		return false, fmt.Errorf("pi.GetAgentHooks: probe pi --version: %w", err)
	}
	version, ok := parsePiVersion(string(out))
	if !ok {
		return false, fmt.Errorf("pi.GetAgentHooks: parse pi --version output %q", strings.TrimSpace(string(out)))
	}
	minimum, _ := parsePiVersion(minPiSettledVersion)
	return !version.less(minimum), nil
}

type piVersion [3]int

func parsePiVersion(output string) (piVersion, bool) {
	match := piVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return piVersion{}, false
	}
	var version piVersion
	for i := range version {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return piVersion{}, false
		}
		version[i] = n
	}
	return version, true
}

func (v piVersion) less(other piVersion) bool {
	for i := range v {
		if v[i] < other[i] {
			return true
		}
		if v[i] > other[i] {
			return false
		}
	}
	return false
}

func piActivityExtensionSource(settledSupported bool) string {
	return `// ` + piExtensionSentinel + ` (do not edit)
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { spawn, spawnSync } from "node:child_process";

const HOOK_TIMEOUT_MS = 5_000;
const AGENT_SETTLED_SUPPORTED = ` + strconv.FormatBool(settledSupported) + `;

function callHookSync(hookName: string, payload: Record<string, unknown>) {
  try {
    const result = spawnSync("ao", ["hooks", "pi", hookName], {
      input: JSON.stringify(payload) + "\n",
      encoding: "utf8",
      stdio: ["pipe", "ignore", "pipe"],
      timeout: HOOK_TIMEOUT_MS,
      windowsHide: true,
    });
    // AO's hook command records daemon delivery failures itself. A missing AO
    // executable or timeout is deliberately ignored so Pi remains usable.
    void result;
  } catch {
    // Activity reporting is best-effort and must never interrupt Pi.
  }
}

function sessionID(ctx: any): string {
  return ctx.sessionManager.getSessionId() ?? "";
}

// Coding Tools' CPA gateway. AO sets these for a session whose model CPA serves; Pi has no
// base-URL variable, so the model is registered here and launched as coding-tools-cpa/<model>.
const GATEWAY_PROVIDER = "` + PiGatewayProvider + `";
const GATEWAY_URL = (process.env.AO_PI_GATEWAY_URL ?? "").trim();
const GATEWAY_MODEL = (process.env.AO_PI_GATEWAY_MODEL ?? "").trim();
const GATEWAY_WINDOW = Number.parseInt(process.env.AO_PI_GATEWAY_CONTEXT_WINDOW ?? "", 10);

function registerGateway(pi) {
  if (!GATEWAY_URL || !GATEWAY_MODEL || typeof pi.registerProvider !== "function") return;
  pi.registerProvider(GATEWAY_PROVIDER, {
    name: "Coding Tools CPA",
    baseUrl: GATEWAY_URL,
    apiKey: "$AO_PI_GATEWAY_API_KEY",
    api: "openai-completions",
    models: [{
      type: "chat", id: GATEWAY_MODEL, name: GATEWAY_MODEL,
      // CPA takes the effort from the model id's "(level)" suffix, so Pi sends none of its own.
      reasoning: false, input: ["text", "image"],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: GATEWAY_WINDOW > 0 ? GATEWAY_WINDOW : 200000, maxTokens: 32000,
    }],
  });
}

// Chat approvals. AO's Pi chat driver sets these for a session on "Ask for approval" or
// "Approve for me" (any mode: AO answers by the chat's current one); a terminal session has
// none, so nothing below runs there. pi-acp sends no ACP permission requests, so each tool
// call but the read-only ones is put to the chat through ` + "`ao hooks pi tool-call`" + `.
// It is spawned asynchronously, never with spawnSync: this runs inside the pi-acp daemon,
// which must keep serving while the person decides.
const APPROVAL_SESSION = (process.env.AO_PI_APPROVAL_SESSION_ID ?? "").trim();
const APPROVAL_TOKEN = (process.env.AO_PI_APPROVAL_TOKEN ?? "").trim();
const APPROVAL_AO = (process.env.AO_PI_APPROVAL_AO_BIN ?? "").trim() || "ao";
const READ_ONLY_TOOLS = new Set(["read", "grep", "find", "ls"]);

// Resolves to { allow, reason }; plain JS (no type annotations), like registerGateway.
function askChat(event) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (answer) => {
      if (!settled) {
        settled = true;
        resolve(answer);
      }
    };
    let out = "";
    let child;
    try {
      child = spawn(APPROVAL_AO, ["hooks", "pi", "tool-call"], {
        stdio: ["pipe", "pipe", "ignore"],
        windowsHide: true,
      });
    } catch (error) {
      finish({ allow: false, reason: "AO could not ask the chat: " + String(error) });
      return;
    }
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      out += chunk;
    });
    child.on("error", (error) => finish({ allow: false, reason: "AO could not ask the chat: " + error.message }));
    child.on("close", () => {
      try {
        const answer = JSON.parse(out.trim().split("\n").pop() ?? "");
        finish({ allow: answer.allow === true, reason: typeof answer.reason === "string" ? answer.reason : undefined });
      } catch {
        finish({ allow: false, reason: "AO gave no answer about this tool call" });
      }
    });
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify({
      tool_call_id: event.toolCallId,
      tool_name: event.toolName,
      input: event.input ?? {},
    }) + "\n");
  });
}

export default function (pi: ExtensionAPI) {
  registerGateway(pi);
  pi.on("tool_call", async (event, _ctx) => {
    if (!APPROVAL_SESSION || !APPROVAL_TOKEN || READ_ONLY_TOOLS.has(event.toolName)) return undefined;
    const answer = await askChat(event);
    if (answer.allow) return undefined;
    return { block: true, reason: answer.reason || "Denied in the chat" };
  });
  pi.on("session_start", async (_event, ctx) => {
    callHookSync("session-start", { session_id: sessionID(ctx) });
  });
  pi.on("before_agent_start", async (event, ctx) => {
    callHookSync("user-prompt-submit", { session_id: sessionID(ctx), prompt: event.prompt });
  });
  // agent_end is the completion event in Pi 0.80.x. Newer releases may retry,
  // compact, or queue follow-up work after it; a subsequent start immediately
  // reactivates AO, while agent_settled below confirms the final idle state.
  pi.on("agent_end", async (_event, ctx) => {
	    if (!AGENT_SETTLED_SUPPORTED) callHookSync("stop", { session_id: sessionID(ctx) });
  });
  pi.on("agent_settled", async (_event, ctx) => {
	    if (AGENT_SETTLED_SUPPORTED) callHookSync("stop", { session_id: sessionID(ctx) });
  });
  pi.on("session_shutdown", async (event, ctx) => {
	    if (event.reason === "quit") {
	      callHookSync("session-end", { session_id: sessionID(ctx), reason: event.reason });
	    }
  });
}
`
}
