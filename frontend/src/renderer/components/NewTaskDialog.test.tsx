import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { NewTaskDialog } from "./NewTaskDialog";
import { STANDALONE_WORKSPACE_ID } from "../types/workspace";

vi.mock("react-i18next", () => ({ useTranslation: () => ({ t: (value: string) => value }) }));
vi.mock("./TaskComposer", () => ({
	TaskComposer: ({ projectId }: { projectId?: string }) => (
		<div data-testid="composer-project">{projectId ?? "no-project"}</div>
	),
}));

describe("new chat quick project selection", () => {
	it("shows a recovery message and keeps the composer unscoped when its project is missing", () => {
		render(<NewTaskDialog open projectUnavailable projects={[{ id: "alpha", name: "Alpha" }]}
			onProjectChange={vi.fn()} onCreated={vi.fn()} onOpenChange={vi.fn()} />);
		expect(screen.getByText(/no longer available/)).toBeTruthy();
		expect(screen.getByTestId("composer-project").textContent).toBe("no-project");
		expect((screen.getByRole("combobox", { name: "Quick project" }) as HTMLSelectElement).value).toBe("");
	});

	it("offers existing projects and Scratchpad without requiring a new window", () => {
		const change = vi.fn();
		render(<NewTaskDialog open projectId={STANDALONE_WORKSPACE_ID}
			projects={[{ id: "alpha", name: "Alpha", path: "/work/alpha" }]}
			onProjectChange={change} onCreated={vi.fn()} onOpenChange={vi.fn()} />);
		const picker = screen.getByRole("combobox", { name: "Quick project" }) as HTMLSelectElement;
		expect(picker.value).toBe(STANDALONE_WORKSPACE_ID);
		fireEvent.change(picker, { target: { value: "alpha" } });
		expect(change).toHaveBeenCalledWith("alpha");
	});
});
