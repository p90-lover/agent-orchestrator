import { createFileRoute } from "@tanstack/react-router";
import { CodingToolsMissionBoard } from "../components/CodingToolsMissionBoard";

export const Route = createFileRoute("/coding-tools-board")({
	validateSearch: (search: Record<string, unknown>) => ({ workspaceId: typeof search.workspaceId === "string" ? search.workspaceId : "" }),
	component: CodingToolsBoardRoute,
});

function CodingToolsBoardRoute() {
	const { workspaceId } = Route.useSearch();
	return <CodingToolsMissionBoard workspaceId={workspaceId} />;
}
