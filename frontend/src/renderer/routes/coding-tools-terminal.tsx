import { createFileRoute } from "@tanstack/react-router";
import { TerminalPane } from "../components/TerminalPane";
import { useResolvedTheme } from "../stores/ui-store";

// `remote=1` marks a session on another machine (the Coding Tools SSH terminal):
// the terminal opens in remote mode. The router decodes search values as JSON,
// so the URL's "1" arrives as the number 1; the string "1" is accepted too.
// Any other value, or none, is a normal local terminal, unless the handle is an
// SSH handle (below).
function isRemoteSearch(value: unknown): boolean {
	return value === 1 || value === "1";
}

// The daemon names every SSH terminal handle with this marker, and protects
// wrapped (`ptyhost-v1:shellterm-ssh-<hex>`), padded and any-case forms too. A
// handle that contains it anywhere is remote whatever `remote` says, so the
// page navigating itself to an SSH handle without remote=1 (or with remote=0)
// still gets remote mode. A false match only adds restrictions.
const SSH_HANDLE_MARKER = "shellterm-ssh-";

function isSshHandle(handle: string): boolean {
	return handle.toLowerCase().includes(SSH_HANDLE_MARKER);
}

// One full-window standalone terminal for a Coding Tools pane (e.g. the Antigravity CLI).
// The host opens the AO shell terminal and passes its handle; this route only renders it.
export const Route = createFileRoute("/coding-tools-terminal")({
	validateSearch: (search: Record<string, unknown>) => {
		const handle = typeof search.handle === "string" ? search.handle : "";
		return {
			handle,
			title: typeof search.title === "string" ? search.title : "Terminal",
			generation: typeof search.generation === "string" ? search.generation : "",
			remote: isRemoteSearch(search.remote) || isSshHandle(handle),
		};
	},
	component: CodingToolsTerminalRoute,
});

function CodingToolsTerminalRoute() {
	const { handle, title, generation, remote } = Route.useSearch();
	const theme = useResolvedTheme();
	if (!handle) {
		return <div className="grid h-screen place-items-center bg-terminal font-mono text-terminal-dim">No terminal selected</div>;
	}
	return (
		<div className="h-screen w-screen overflow-hidden bg-terminal">
			<TerminalPane
				daemonReady
				fontSize={13}
				theme={theme}
				terminalTarget={{
					kind: "shell",
					handleId: handle,
					title,
					generation: generation || handle,
					...(remote ? { remote: true } : {}),
				}}
			/>
		</div>
	);
}
