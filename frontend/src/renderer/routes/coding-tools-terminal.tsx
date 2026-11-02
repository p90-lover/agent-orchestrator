import { createFileRoute } from "@tanstack/react-router";
import { TerminalPane } from "../components/TerminalPane";
import { useResolvedTheme } from "../stores/ui-store";

// One full-window standalone terminal for a Coding Tools pane (e.g. the Antigravity CLI).
// The host opens the AO shell terminal and passes its handle; this route only renders it.
export const Route = createFileRoute("/coding-tools-terminal")({
	validateSearch: (search: Record<string, unknown>) => ({
		handle: typeof search.handle === "string" ? search.handle : "",
		title: typeof search.title === "string" ? search.title : "Terminal",
		generation: typeof search.generation === "string" ? search.generation : "",
	}),
	component: CodingToolsTerminalRoute,
});

function CodingToolsTerminalRoute() {
	const { handle, title, generation } = Route.useSearch();
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
				terminalTarget={{ kind: "shell", handleId: handle, title, generation: generation || handle }}
			/>
		</div>
	);
}
