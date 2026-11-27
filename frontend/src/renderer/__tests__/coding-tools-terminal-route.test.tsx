import { render, screen } from "@testing-library/react";
import type { ComponentType } from "react";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import type { TerminalTarget } from "../types/terminal";

const routeMocks = vi.hoisted(() => ({
	search: {} as Record<string, unknown>,
	paneTargets: [] as Array<TerminalTarget | undefined>,
}));

// The route is rendered without a router: useSearch runs the route's own
// validateSearch over the raw (already JSON-decoded) search values, the way
// TanStack Router hands them over.
vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...(await importOriginal<typeof import("@tanstack/react-router")>()),
	createFileRoute:
		() =>
		(options: { validateSearch: (search: Record<string, unknown>) => unknown }) => ({
			options,
			useSearch: () => options.validateSearch(routeMocks.search),
		}),
}));

vi.mock("../components/TerminalPane", () => ({
	TerminalPane: (props: { terminalTarget?: TerminalTarget }) => {
		routeMocks.paneTargets.push(props.terminalTarget);
		return <div data-testid="terminal-pane" />;
	},
}));

import { Route } from "../routes/coding-tools-terminal";

// The router plugin code-splits route components (autoCodeSplitting), so this is
// a lazy component: the first render suspends until its chunk loads.
const CodingToolsTerminalRoute = Route.options.component as ComponentType;

async function renderedTarget(search: Record<string, unknown>): Promise<TerminalTarget | undefined> {
	routeMocks.search = search;
	render(<CodingToolsTerminalRoute />);
	expect(await screen.findByTestId("terminal-pane")).toBeInTheDocument();
	return routeMocks.paneTargets.at(-1);
}

// A local (non-SSH) shell handle; SSH handles carry the daemon's `shellterm-ssh-` prefix.
const base = { handle: "shellterm-local-1", title: "Build server", generation: "2026-11-27T10:00:00Z" };

describe("coding-tools-terminal route", () => {
	beforeAll(async () => {
		// Load the split chunk once up front so no case races its first import.
		await (CodingToolsTerminalRoute as { preload?: () => Promise<unknown> }).preload?.();
	});

	beforeEach(() => {
		routeMocks.search = {};
		routeMocks.paneTargets.length = 0;
	});

	// TanStack Router decodes `remote=1` to the number 1; a quoted "1" stays a string.
	it.each([1, "1"])("opens the shell terminal in remote mode for remote=%j", async (remote) => {
		expect(await renderedTarget({ ...base, remote })).toEqual({
			kind: "shell",
			handleId: base.handle,
			title: base.title,
			generation: base.generation,
			remote: true,
		});
	});

	it.each([undefined, 0, "0", 2, true, "true", "yes", "", null])(
		"opens a normal local terminal for remote=%j",
		async (remote) => {
			const search = remote === undefined ? { ...base } : { ...base, remote };
			// Exactly the target the route built before remote mode existed.
			expect(await renderedTarget(search)).toEqual({
				kind: "shell",
				handleId: base.handle,
				title: base.title,
				generation: base.generation,
			});
			expect(routeMocks.paneTargets.at(-1)).not.toHaveProperty("remote");
		},
	);

	// The page navigating itself to an SSH handle must not get a local terminal.
	it.each([
		["no remote param", "shellterm-ssh-1", undefined],
		["remote=0", "shellterm-ssh-1", 0],
		["remote=\"0\"", "shellterm-ssh-1", "0"],
		["a mixed-case prefix", "ShellTerm-SSH-1", undefined],
		["an upper-case prefix and remote=1", "SHELLTERM-SSH-2", 1],
		// The marker anywhere in the handle counts: wrapped, padded or prefixed forms.
		["a pty-host wrapped handle", "ptyhost-v1:shellterm-ssh-1", undefined],
		["an upper-case wrapped handle", "PTYHOST-V1:SHELLTERM-SSH-1", undefined],
		["a padded handle", " shellterm-ssh-1", undefined],
		["a prefixed handle", "x-shellterm-ssh-1", undefined],
	])("opens an SSH handle in remote mode with %s", async (_label, handle, remote) => {
		const search = remote === undefined ? { ...base, handle } : { ...base, handle, remote };
		expect(await renderedTarget(search)).toEqual({
			kind: "shell",
			handleId: handle,
			title: base.title,
			generation: base.generation,
			remote: true,
		});
	});

	it.each(["shellterm-local-1", "shellterm-ssh", "shellterm-sshx-1"])(
		"keeps the exact old target for the non-SSH handle %j without a remote param",
		async (handle) => {
			expect(await renderedTarget({ ...base, handle })).toEqual({
				kind: "shell",
				handleId: handle,
				title: base.title,
				generation: base.generation,
			});
			expect(routeMocks.paneTargets.at(-1)).not.toHaveProperty("remote");
		},
	);

	it("renders no terminal without a handle, even in remote mode", async () => {
		routeMocks.search = { remote: 1 };
		render(<CodingToolsTerminalRoute />);
		expect(await screen.findByText("No terminal selected")).toBeInTheDocument();
		expect(routeMocks.paneTargets).toHaveLength(0);
	});
});
