import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { useWorkspaceQuery, workspaceQueryKey } from "../hooks/useWorkspaceQuery";
import { useUiStore } from "../stores/ui-store";
import { NewTaskDialog } from "./NewTaskDialog";
import { STANDALONE_PROJECT_KIND, STANDALONE_WORKSPACE_ID } from "../types/workspace";

// App-level New Task surface. Lives in the shell (always mounted, on every
// route and platform, unlike ShellTopbar which unmounts on Linux boards) so a
// ⌘N / Ctrl+Shift+N shortcut or the sidebar "New session" item can open a
// worker for any project — including one you are not currently viewing.
// Driven by the ui-store `newTaskRequest` signal; the nonce lets a repeat
// request for the same project re-open the dialog.
export function GlobalNewTaskDialog() {
	const navigate = useNavigate();
	const queryClient = useQueryClient();
	const newTaskRequest = useUiStore((state) => state.newTaskRequest);
	const [open, setOpen] = useState(false);
	const [projectId, setProjectId] = useState<string | undefined>(undefined);
	const lastNonce = useRef(newTaskRequest?.nonce ?? 0);
	const projectsQuery = useWorkspaceQuery({ subscribed: open });
	const projects = (projectsQuery.data ?? [])
		.filter((workspace) => workspace.kind !== STANDALONE_PROJECT_KIND)
		.map((workspace) => ({ id: workspace.id, name: workspace.name, path: workspace.path }));
	const projectAvailable = projectId === STANDALONE_WORKSPACE_ID ||
		projects.some((project) => project.id === projectId);
	// Never hand an unknown project to the composer: it otherwise renders a
	// seemingly working input with its Send action permanently disabled.
	const selectedProjectId = projectAvailable ? projectId : undefined;

	useEffect(() => {
		if (!newTaskRequest || newTaskRequest.nonce === lastNonce.current) return;
		lastNonce.current = newTaskRequest.nonce;
		// Consume requests that arrive while this dialog is already open. In
		// particular, do not retarget a populated form to another project, and do
		// not replay the ignored request when the user later closes the dialog.
		if (open) return;
		setProjectId(newTaskRequest.projectId || STANDALONE_WORKSPACE_ID);
		setOpen(true);
	}, [newTaskRequest, open]);

	const handleCreated = async (sessionId: string) => {
		if (!selectedProjectId) return;
		await queryClient.invalidateQueries({ queryKey: workspaceQueryKey });
		if (selectedProjectId === STANDALONE_WORKSPACE_ID) {
			void navigate({ to: "/sessions/$sessionId", params: { sessionId } });
			return;
		}
		void navigate({
			to: "/projects/$projectId/sessions/$sessionId",
			params: { projectId: selectedProjectId, sessionId },
		});
	};

	return (
		<NewTaskDialog
			open={open}
			projectId={selectedProjectId}
			projects={projects}
			onProjectChange={setProjectId}
			projectsLoading={projectsQuery.isPending}
			projectUnavailable={Boolean(projectId) && !projectAvailable && !projectsQuery.isPending}
			onCreated={(sessionId) => void handleCreated(sessionId)}
			onOpenChange={setOpen}
		/>
	);
}
