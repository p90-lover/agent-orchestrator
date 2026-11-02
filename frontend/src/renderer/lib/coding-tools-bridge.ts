import type { AoBridge } from "../../preload";
import { coerceUiSettings, DEFAULT_UI_SETTINGS } from "../../shared/ui-locale";

/** The renderer is hosted inside a Coding Tools pane: no native window, menus or mobile pairing. */
export const CODING_TOOLS_EMBEDDED = import.meta.env.VITE_CODING_TOOLS_EMBEDDED === "1";

async function hostRequest(operation: string, args: unknown = {}) {
	const response = await fetch("/integration/desktop", {
		method: "POST", headers: { "content-type": "application/json" },
		body: JSON.stringify({ operation, args }), credentials: "same-origin",
	});
	const result = await response.json();
	if (!response.ok || result.ok === false) throw new Error(result.error || "Coding Tools host request failed");
	return result.value;
}

async function readStatus() {
	const response = await fetch("/integration/status", { credentials: "same-origin" });
	if (!response.ok) throw new Error("Coding Tools AO runtime is unavailable");
	return response.json();
}

export function readCodingToolsMissions(workspaceId: string) {
	return hostRequest("mission_board", { workspaceId });
}

export function openCodingToolsMission(workspaceId: string, runId: string, intent: "open" | "start" | "resume" | "restart") {
	return hostRequest("mission_open", { workspaceId, runId, intent });
}

const unavailable = async () => {
	throw new Error("This desktop capability is not exposed by the local-only Coding Tools integration.");
};

export function createCodingToolsBridge(base: AoBridge): AoBridge {
	return {
		...base,
		app: {
			...base.app,
			getVersion: () => hostRequest("version"),
			chooseDirectory: () => hostRequest("choose_directory"),
			checkGitRepository: (directory) => hostRequest("check_repository", { directory }),
			getRepositoryBranch: (directory) => hostRequest("repository_branch", { directory }),
			openExternal: (url) => hostRequest("open_external", { url }),
			scanImportFolder: unavailable,
			checkAncestorRepo: unavailable,
			checkGitHubRepositoryAvailability: unavailable,
			getPathForFile: () => { throw new Error("Use Add project to select a local directory."); },
		},
		daemon: {
			getStatus: readStatus,
			start: () => hostRequest("start"),
			stop: () => hostRequest("stop"),
			restart: () => hostRequest("restart"),
			onStatus: () => () => undefined,
		},
		appState: { getMigration: async () => ({ status: "declined" as const }), setMigration: unavailable },
		uiSettings: {
			get: async () => coerceUiSettings(JSON.parse(localStorage.getItem("coding-tools-ao-settings") || "null") ?? DEFAULT_UI_SETTINGS),
			set: async (settings) => {
				const stored = JSON.parse(localStorage.getItem("coding-tools-ao-settings") || "null") ?? DEFAULT_UI_SETTINGS;
				const next = coerceUiSettings({ ...stored, ...settings });
				localStorage.setItem("coding-tools-ao-settings", JSON.stringify(next));
				return next;
			},
		},
		keybindings: {
			...base.keybindings,
			get: async () => JSON.parse(localStorage.getItem("coding-tools-ao-keys") || "{}"),
			set: async (overrides) => { localStorage.setItem("coding-tools-ao-keys", JSON.stringify(overrides)); return overrides; },
		},
		remotes: { list: async () => [], add: unavailable, update: unavailable, remove: unavailable, probe: unavailable, request: unavailable },
		cloud: { ...base.cloud, signIn: unavailable, signOut: unavailable, connectProviderAuth: unavailable },
		updates: { ...base.updates, check: unavailable, download: unavailable, install: unavailable, relaunch: unavailable },
		updateSettings: { ...base.updateSettings, set: unavailable, setMacDifferentialUpdates: unavailable },
		browserProfiles: { ...base.browserProfiles, create: unavailable, rename: unavailable, clear: unavailable, delete: unavailable, import: unavailable },
		browser: { ...base.browser, nativeCompositionEnabled: false, ensure: unavailable, navigate: unavailable, openTab: unavailable },
		terminal: { ...base.terminal, saveDroppedFile: unavailable },
	};
}
