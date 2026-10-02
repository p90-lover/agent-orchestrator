import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Bot } from "lucide-react";
import { SessionCardView, SessionsBoardGridView, type BoardSessionPresentation } from "@aoagents/product-ui";
import { readCodingToolsMissions, openCodingToolsMission } from "../lib/coding-tools-bridge";
import { boardKanbanColumnOrder, getKanbanColumnView } from "../lib/session-presentation";
import { useTranslation } from "react-i18next";
import { ProductExternalLink } from "./ProductExternalLink";
import { Button } from "./ui/button";

type Mission = {
	id: string; project_id: string; workspace_id: string; revision: number; cancelled: boolean; paused?: boolean;
	nodes: { id: string; role: string; state: string; receipt?: { error?: string } }[];
};
type Board = { workspaceId: string; projectId: string | null; runs: Mission[]; tasks: { id: string; title: string; updated_at?: number }[] };
type Card = BoardSessionPresentation & { mission: Mission; state: string };

export function missionCard(mission: Mission, task?: Board["tasks"][number]): Card {
	const state = mission.nodes.some(node => node.role === "reviewer" && ["running", "reserved"].includes(node.state)) ? "review"
		: mission.nodes.some(node => ["running", "reserved"].includes(node.state)) ? "running"
		: mission.cancelled ? "cancelled"
		: mission.nodes.some(node => node.state === "held") ? "held"
		: mission.nodes.length > 0 && mission.nodes.every(node => node.state === "finished") ? "finished"
		: mission.nodes.length > 0 && mission.nodes.every(node => ["archived", "cancelled"].includes(node.state)) ? "archived" : "pending";
	return {
		id: mission.id, title: task?.title || mission.id, mission, state, provider: "chatgpt-web",
		status: state === "held" ? "needs_input" : ["running", "review"].includes(state) ? "working" : "idle",
		kanbanColumn: ["finished", "cancelled", "archived"].includes(state) ? "ready" : ["review", "held"].includes(state) ? "needs_review" : state === "running" ? "validating" : "building",
		displayStatus: mission.paused && !mission.cancelled ? "Paused" : state[0].toUpperCase() + state.slice(1),
		updatedAt: task?.updated_at ? new Date(task.updated_at).toISOString() : "",
	};
}

export function CodingToolsMissionBoard({ workspaceId }: { workspaceId: string }) {
	const { t } = useTranslation();
	const [showInactive, setShowInactive] = useState(false);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState("");
	const query = useQuery({
		queryKey: ["coding-tools-missions", workspaceId], retry: false, refetchInterval: 3000,
		queryFn: async (): Promise<Board> => {
			const result = await readCodingToolsMissions(workspaceId);
			if (!result?.ok || result.workspaceId !== workspaceId || !Array.isArray(result.runs) || !Array.isArray(result.tasks)) throw new Error("Workspace mission board is unavailable");
			return result;
		},
	});
	const labels: Record<string, string> = { building: "Pending", validating: "Running", needs_review: "Review", ready: "Finished" };
	const columns = boardKanbanColumnOrder.map(column => { const view = getKanbanColumnView(column, t); return { ...view, label: labels[column] ?? view.label }; });
	const rows = (query.data?.runs ?? []).map(mission => missionCard(mission, query.data?.tasks.find(task => task.id === mission.project_id)))
		.filter(card => showInactive || !["cancelled", "archived"].includes(card.state));
	const open = async (card: Card, intent: "open" | "start" | "resume") => {
		if (busy) return;
		setBusy(card.id); setError("");
		try { await openCodingToolsMission(workspaceId, card.id, intent); }
		catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
		finally { setBusy(""); }
	};
	return <section className="flex h-screen min-h-0 flex-col bg-background text-foreground" aria-label="AO workspace mission board">
		<div className="flex shrink-0 items-center justify-between gap-3 px-4 py-2 text-xs">
			<strong>Workspace missions</strong>
			<label className="flex items-center gap-2"><input type="checkbox" checked={showInactive} onChange={event => setShowInactive(event.target.checked)} />Show cancelled and archived</label>
		</div>
		{error || query.error ? <p role="alert" className="px-4 py-2 text-sm text-destructive">{error || query.error?.message}</p> : null}
		{query.isPending ? <p role="status" className="p-4 text-sm text-muted-foreground">Loading missions</p> : null}
		{query.isSuccess && rows.length === 0 ? <p className="p-4 text-sm text-muted-foreground">No missions in this workspace. Use New mission to add one.</p> : null}
		<div className="min-h-0 flex-1">
			<SessionsBoardGridView columns={columns} labels={{ columnAria: label => `${label} missions` }} sessions={rows} renderSessionCard={card => (
				<div className={["finished", "held"].includes(card.state) ? "opacity-75" : ""} data-mission-id={card.id}>
					<SessionCardView session={card} onOpen={() => void open(card, "open")}
						externalLink={ProductExternalLink} renderAvatar={() => <Bot className="size-4" aria-hidden="true" />}
						error={card.mission.nodes.find(node => node.state === "held")?.receipt?.error}
						labels={{ formatTime: () => "", updatedAt: () => "", intakeIssue: id => id, pr: { short: "PR", states: { open: "Open", closed: "Closed", draft: "Draft", merged: "Merged" } } }}
						footer={<span className="text-xs text-muted-foreground">{card.mission.cancelled ? "Future stages cancelled · " : ""}{card.mission.nodes.filter(node => node.state === "finished").length}/{card.mission.nodes.length} stages finished</span>}
						action={!card.mission.cancelled && (card.state === "pending" || card.mission.paused) ? <Button size="sm" disabled={Boolean(busy)} onClick={event => { event.stopPropagation(); void open(card, card.mission.paused ? "resume" : "start"); }}>{card.mission.paused ? "Resume mission" : "Start mission"}</Button> : undefined}
					/>
				</div>
			)} />
		</div>
	</section>;
}
