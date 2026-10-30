import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { SessionCardView, SessionsBoardGridView, type BoardSessionPresentation } from "@aoagents/product-ui";
import { readCodingToolsMissions, openCodingToolsMission } from "../lib/coding-tools-bridge";
import { boardKanbanColumnOrder, getKanbanColumnView } from "../lib/session-presentation";
import { useTranslation } from "react-i18next";
import { ProductExternalLink } from "./ProductExternalLink";
import { Button } from "./ui/button";
import { AgentAvatar } from "./AgentAvatar";
import { sessionUsageQueryOptions, type SessionUsageSummary } from "../hooks/useSessionUsageSummaries";
import { formatCostNanos } from "../lib/format-cost";
import { formatTokenCount } from "../lib/format-token-count";
import { formatTimeCompact } from "../lib/format-time";

type Route = { harness_id?: string; model?: string };
type Receipt = { error?: string; thread_id?: string | null; route?: Route; started_at_ms?: number };
type Mission = {
	id: string; project_id: string; workspace_id: string; revision: number; cancelled: boolean; paused?: boolean;
	nodes: { id: string; role: string; state: string; route?: Route; receipt?: Receipt | null; history?: Receipt[] }[];
};
type Board = { workspaceId: string; projectId: string | null; runs: Mission[]; tasks: { id: string; title: string; updated_at?: number }[] };
type MissionRoute = { harness: string; model: string; provider: string; label: string; configured: boolean };
type Card = BoardSessionPresentation & {
	mission: Mission; state: string; routes: MissionRoute[]; sessionIds: string[];
	usage: { compactLabel: string; accessibleLabel: string }; costCoverage: string;
	startedAt: string | null; elapsedLabel: string | null;
};

export function missionCard(mission: Mission, task?: Board["tasks"][number], usageBySession?: ReadonlyMap<string, SessionUsageSummary>, now = Date.now()): Card {
	const state = mission.nodes.some(node => node.role === "reviewer" && ["running", "reserved"].includes(node.state)) ? "review"
		: mission.nodes.some(node => ["running", "reserved"].includes(node.state)) ? "running"
		: mission.cancelled ? "cancelled"
		: mission.nodes.some(node => node.state === "held") ? "held"
		: mission.nodes.length > 0 && mission.nodes.every(node => node.state === "finished") ? "finished"
		: mission.nodes.length > 0 && mission.nodes.every(node => ["archived", "cancelled"].includes(node.state)) ? "archived" : "pending";
	const routes = new Map<string, MissionRoute>();
	const sessions = new Set<string>();
	const starts: number[] = [];
	const activeStarts: number[] = [];
	let nativeWebUsage = false;
	let otherUnmeteredUsage = false;
	for (const node of mission.nodes) {
		const receipts = [...(node.history ?? []), ...(node.receipt ? [node.receipt] : [])];
		for (const attempt of receipts) {
			if (attempt.route?.harness_id?.startsWith("ao:") && attempt.thread_id) sessions.add(attempt.thread_id);
			else if (attempt.route?.harness_id === "codex-native" && attempt.route.model?.startsWith("chatgpt-web/")) nativeWebUsage = true;
			else otherUnmeteredUsage = true;
			if (typeof attempt.started_at_ms === "number" && attempt.started_at_ms > 0 && Number.isFinite(new Date(attempt.started_at_ms).getTime())) starts.push(attempt.started_at_ms);
		}
		const activeStart = node.receipt?.started_at_ms;
		if (["running", "reserved"].includes(node.state) && typeof activeStart === "number" && activeStart > 0 && Number.isFinite(new Date(activeStart).getTime())) activeStarts.push(activeStart);
		// Receipt snapshots are historical facts; today's edited route is only a fallback for an unrun card.
		for (const route of receipts.length ? receipts.map(attempt => attempt.route) : [node.route]) {
			const harness = route?.harness_id || "unknown";
			const model = route?.model || "Model unavailable";
			const provider = harness.startsWith("ao:") ? harness.slice(3) : harness === "codex-native" ? model.startsWith("chatgpt-web/") ? "chatgpt-web" : "codex" : harness;
			const label = harness === "codex-native" ? provider === "chatgpt-web" ? "Native WebGPT" : "Native Codex" : harness.startsWith("ao:") ? harness.slice(3) : "Unknown harness";
			const configured = receipts.length === 0;
			routes.set(`${harness}\0${model}\0${configured}`, { harness, model, provider, label, configured });
		}
	}
	const sessionIds = [...sessions];
	let totalNanos = 0;
	let priced = 0;
	let processedTokens = 0;
	let tokenSessions = 0;
	let partial = nativeWebUsage || otherUnmeteredUsage;
	let tokenPartial = partial;
	for (const id of sessionIds) {
		const usage = usageBySession?.get(id);
		const tokens = usage?.processedTokens;
		if (typeof tokens === "number" && Number.isFinite(tokens) && tokens >= 0) {
			processedTokens += tokens;
			tokenSessions++;
			tokenPartial ||= usage?.incomplete === true;
		} else tokenPartial = true;
		const cost = usage?.estimatedCost;
		if (usage && cost && formatCostNanos(cost.totalNanos) !== null) {
			totalNanos += cost.totalNanos;
			priced++;
			partial ||= cost.coverage !== "complete" || usage.incomplete;
		} else partial = true;
	}
	const cost = priced ? formatCostNanos(totalNanos) : null;
	// Like AO's session board, retain measured tokens when this provider has no dollar estimate.
	const tokens = tokenSessions ? `${formatTokenCount(processedTokens)}${tokenPartial ? " · partial" : ""}` : null;
	const tokenDetail = tokenSessions ? `${processedTokens.toLocaleString("en-US")} recorded tokens${tokenPartial ? " · Partial token coverage" : ""}` : "Token usage not reported";
	const costCoverage = [
		sessionIds.length ? `${priced}/${sessionIds.length} AO sessions priced` : null,
		nativeWebUsage ? "Native WebGPT cost unavailable" : null,
		otherUnmeteredUsage ? "Other attempt usage unavailable" : null,
	].filter(Boolean).join(" · ") || "No recorded session usage";
	const providers = new Set([...routes.values()].map(route => route.provider));
	const elapsed = activeStarts.length ? Math.max(0, Math.floor((now - Math.min(...activeStarts)) / 1000)) : null;
	const elapsedLabel = elapsed === null ? null : elapsed >= 3600 ? `${Math.floor(elapsed / 3600)}h ${Math.floor(elapsed % 3600 / 60)}m` : elapsed >= 60 ? `${Math.floor(elapsed / 60)}m ${elapsed % 60}s` : `${elapsed}s`;
	return {
		id: mission.id, title: task?.title || mission.id, mission, state, provider: providers.size > 1 ? "mixed" : [...providers][0] || "unknown",
		routes: [...routes.values()], sessionIds, costCoverage,
		usage: { compactLabel: cost === null ? tokens ?? "Usage not reported" : `Est. ${cost}${partial ? " · partial" : ""}`, accessibleLabel: `${cost === null ? "USD estimate not reported" : `Estimated cost to date ${cost}${partial ? " · Partial coverage" : ""}`} · ${tokenDetail} · ${costCoverage}` },
		startedAt: starts.length ? new Date(Math.min(...starts)).toISOString() : null, elapsedLabel,
		status: state === "held" ? "needs_input" : ["running", "review"].includes(state) ? "working" : "idle",
		kanbanColumn: ["finished", "cancelled", "archived"].includes(state) ? "ready" : ["review", "held"].includes(state) ? "needs_review" : state === "running" ? "validating" : "building",
		displayStatus: mission.paused && !mission.cancelled ? "Paused" : state[0].toUpperCase() + state.slice(1),
		// integrations::board::Task uses Unix seconds; receipt.started_at_ms above is already milliseconds.
		updatedAt: task?.updated_at ? new Date(task.updated_at * 1000).toISOString() : "",
	};
}

export function CodingToolsMissionBoard({ workspaceId }: { workspaceId: string }) {
	const { t } = useTranslation();
	const [showInactive, setShowInactive] = useState(false);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState("");
	const [now, setNow] = useState(Date.now);
	const query = useQuery({
		queryKey: ["coding-tools-missions", workspaceId], retry: false, refetchInterval: 3000,
		queryFn: async (): Promise<Board> => {
			const result = await readCodingToolsMissions(workspaceId);
			if (!result?.ok || result.workspaceId !== workspaceId || !Array.isArray(result.runs) || !Array.isArray(result.tasks)) throw new Error("Workspace mission board is unavailable");
			return result;
		},
	});
	const usageQuery = useQuery({
		...sessionUsageQueryOptions(query.data?.projectId ?? undefined),
		enabled: Boolean(query.data?.projectId),
	});
	const labels: Record<string, string> = { building: "Pending", validating: "Running", needs_review: "Review", ready: "Finished" };
	const columns = boardKanbanColumnOrder.map(column => { const view = getKanbanColumnView(column, t); return { ...view, label: labels[column] ?? view.label }; });
	const rows = (query.data?.runs ?? []).map(mission => missionCard(mission, query.data?.tasks.find(task => task.id === mission.project_id), query.data?.projectId ? usageQuery.data : undefined, now))
		.filter(card => showInactive || !["cancelled", "archived"].includes(card.state));
	const hasActiveClock = rows.some(card => card.elapsedLabel !== null);
	useEffect(() => {
		if (!hasActiveClock) return;
		setNow(Date.now());
		const timer = setInterval(() => setNow(Date.now()), 1000);
		return () => clearInterval(timer);
	}, [hasActiveClock]);
	const open = async (card: Card, intent: "open" | "start" | "resume" | "restart") => {
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
						externalLink={ProductExternalLink} renderAvatar={() => <span className="flex shrink-0 items-center gap-1">{[...new Set(card.routes.map(route => route.provider))].map(provider => <AgentAvatar key={provider} provider={provider} className="size-4" />)}</span>}
						usage={card.usage}
						error={card.mission.nodes.find(node => node.state === "held")?.receipt?.error}
						labels={{ formatTime: timestamp => timestamp ? formatTimeCompact(timestamp) : "Time unavailable", updatedAt: timestamp => timestamp ? `Task updated ${formatTimeCompact(timestamp)}` : "Task update time unavailable", intakeIssue: id => id, pr: { short: "PR", states: { open: "Open", closed: "Closed", draft: "Draft", merged: "Merged" } } }}
						footer={<div className="space-y-1.5 px-3.5 pb-2.5 text-2xs text-muted-foreground">
							<div className="space-y-1">{card.routes.map(route => <div key={`${route.harness}:${route.model}:${route.configured}`} className="break-words">{route.label} · {route.model}{route.configured ? " · configured" : ""}</div>)}</div>
							<div>{card.costCoverage}</div>
							<div title="Start times are reservation times, not completion times. Elapsed covers only the current active attempt.">{card.startedAt ? `Started ${formatTimeCompact(card.startedAt)}` : "Start time unavailable"}{card.elapsedLabel ? ` · Active ${card.elapsedLabel} since reservation` : ""}</div>
							<div>{card.mission.cancelled ? "Future stages cancelled · " : ""}{card.mission.nodes.filter(node => node.state === "finished").length}/{card.mission.nodes.length} stages finished</div>
						</div>}
						action={["running", "review"].includes(card.state) ? undefined : <Button size="sm" disabled={Boolean(busy)} onClick={event => { event.stopPropagation(); void open(card, card.mission.paused && !card.mission.cancelled ? "resume" : card.state === "pending" || card.state === "held" ? "start" : "restart"); }}>{card.mission.paused && !card.mission.cancelled ? "Resume mission" : card.state === "pending" ? "Start mission" : card.state === "held" ? "Retry mission" : "Restart mission"}</Button>}
					/>
				</div>
			)} />
		</div>
	</section>;
}
