import * as Dialog from "@radix-ui/react-dialog";
import { useTranslation } from "react-i18next";
import { TaskComposer } from "./TaskComposer";
import { STANDALONE_WORKSPACE_ID } from "../types/workspace";

type NewTaskDialogProps = {
	open: boolean;
	projectId?: string;
	projects?: readonly { id: string; name: string; path?: string }[];
	onProjectChange?: (projectId: string) => void;
	projectsLoading?: boolean;
	projectUnavailable?: boolean;
	onCreated: (sessionId: string) => void;
	onOpenChange: (open: boolean) => void;
};

export function NewTaskDialog({
	open, projectId, projects = [], onProjectChange, projectsLoading = false,
	projectUnavailable = false, onCreated, onOpenChange,
}: NewTaskDialogProps) {
	const { t } = useTranslation();
	return (
		<Dialog.Root open={open} onOpenChange={onOpenChange}>
			<Dialog.Portal>
				<Dialog.Overlay className="dialog-overlay data-[state=open]:animate-overlay-in data-[state=closed]:animate-overlay-out" />
				<Dialog.Content className="fixed left-1/2 top-1/2 z-overlay w-dialog-xl max-w-[calc(100vw-2rem)] max-h-[calc(100vh-2rem)] -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-lg border border-border bg-popover p-0 text-popover-foreground shadow-xl data-[state=open]:animate-modal-in data-[state=closed]:animate-modal-out motion-reduce:animate-none">
					{/* One title line names the dialog, styled like every other settings-style
					    modal; everything else stays the composer's surface, no bordered header. */}
					<Dialog.Title className="settings-dialog-title px-4 pt-3">{t("newTask.title")}</Dialog.Title>
					<Dialog.Description className="sr-only">
						{t(projectId === STANDALONE_WORKSPACE_ID ? "newTask.standaloneDescription" : "newTask.description")}
					</Dialog.Description>
					{onProjectChange ? (
						<div className="flex flex-wrap items-center gap-2 px-4 pt-3">
							<label htmlFor="new-task-quick-project" className="shrink-0 text-sm text-muted-foreground">
								Quick project
							</label>
							<select
								id="new-task-quick-project"
								aria-label="Quick project"
								className="min-w-0 flex-1 rounded-md border border-border bg-background px-2 py-1.5 text-sm text-foreground"
								value={projectId ?? ""}
								onChange={(event) => onProjectChange(event.target.value)}
							>
								{!projectId ? <option value="">{projectsLoading ? "Loading projects…" : "Select a project"}</option> : null}
								<option value={STANDALONE_WORKSPACE_ID}>Scratchpad (no project)</option>
								{projects.map((project) => (
									<option key={project.id} value={project.id} title={project.path}>{project.name}</option>
								))}
							</select>
						</div>
					) : null}
					{!projectId && onProjectChange ? (
						<p role="status" className="px-4 pt-2 text-xs text-muted-foreground">
							{projectsLoading ? "Loading available projects…" : projectUnavailable
								? "This project is no longer available. Select a project or Scratchpad to start chatting."
								: "Select a project or Scratchpad to enable chat."}
						</p>
					) : null}
					<TaskComposer
						key={projectId ?? "select-project"}
						projectId={projectId}
						autoFocusTitle
						onCreated={(sessionId) => {
							onCreated(sessionId);
							onOpenChange(false);
						}}
					/>
				</Dialog.Content>
			</Dialog.Portal>
		</Dialog.Root>
	);
}
