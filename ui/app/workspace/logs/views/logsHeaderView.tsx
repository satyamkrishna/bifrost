import { ColumnConfigDropdown, type ColumnConfigEntry } from "@/components/table";
import { Button } from "@/components/ui/button";
import { Command, CommandItem, CommandList } from "@/components/ui/command";
import { DateTimePickerWithRange } from "@/components/ui/datePickerWithRange";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useTimezonePreference } from "@/lib/hooks/useTimezonePreference";
import { getErrorMessage } from "@/lib/store";
import { getActiveTempToken } from "@/lib/store/apis/tempToken";
import type { LogFilters as LogFiltersType, RecalculateCostProgress, RecalculateCostResponse } from "@/lib/types/logs";
import { getApiBaseUrl } from "@/lib/utils/port";
import { getRangeForPeriod, TIME_PERIODS } from "@/lib/utils/timeRange";
import { formatCompactNumber } from "@/lib/utils/numbers";
import { Calculator, Check, MoreVertical, Radio, RefreshCw, Search } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

type RecalculateCostMode = "missing" | "all";

interface LogsHeaderViewProps {
	filters: LogFiltersType;
	onFiltersChange: (filters: LogFiltersType) => void;
	fetchLogs: () => Promise<void>;
	fetchStats: () => Promise<void>;
	fetchHistogram: () => Promise<void>;
	loading?: boolean;
	polling: boolean;
	onPollToggle: (enabled: boolean) => void;
	period: string;
	onPeriodChange: (period?: string, from?: Date, to?: Date) => void;
	/** Total logs matching the current filters/time window (stats.total_requests) */
	totalLogs: number;
	/** Column config for the ColumnConfigDropdown */
	columnEntries: ColumnConfigEntry[];
	columnLabels: Record<string, string>;
	onToggleColumnVisibility: (id: string) => void;
	onResetColumns: () => void;
}

export function LogsHeaderView({
	filters,
	onFiltersChange,
	fetchLogs,
	fetchStats,
	fetchHistogram,
	loading = false,
	polling,
	onPollToggle,
	period,
	onPeriodChange,
	totalLogs,
	columnEntries,
	columnLabels,
	onToggleColumnVisibility,
	onResetColumns,
}: LogsHeaderViewProps) {
	const [openMoreActionsPopover, setOpenMoreActionsPopover] = useState(false);
	const [recalcDialogOpen, setRecalcDialogOpen] = useState(false);
	const [recalcMode, setRecalcMode] = useState<RecalculateCostMode>("missing");
	const [localSearch, setLocalSearch] = useState(filters.content_search || "");
	const searchTimeoutRef = useRef<NodeJS.Timeout | undefined>(undefined);
	const filtersRef = useRef<LogFiltersType>(filters);

	const [timezone, setTimezone] = useTimezonePreference();

	const [startTime, setStartTime] = useState<Date | undefined>(filters.start_time ? new Date(filters.start_time) : undefined);
	const [endTime, setEndTime] = useState<Date | undefined>(filters.end_time ? new Date(filters.end_time) : undefined);

	useEffect(() => {
		setStartTime(filters.start_time ? new Date(filters.start_time) : undefined);
		setEndTime(filters.end_time ? new Date(filters.end_time) : undefined);
	}, [filters.start_time, filters.end_time]);

	useEffect(() => {
		filtersRef.current = filters;
	}, [filters]);

	useEffect(() => {
		setLocalSearch(filters.content_search || "");
	}, [filters.content_search]);

	useEffect(() => {
		return () => {
			if (searchTimeoutRef.current) clearTimeout(searchTimeoutRef.current);
		};
	}, []);

	const handleRecalculateCosts = useCallback(async () => {
		setRecalcDialogOpen(false);
		const missingCostOnly = recalcMode === "missing";
		const toastId = "logs-recalculate-costs";
		const recalculatePromise = recalculateCostsWithProgress(filters, missingCostOnly, (progress) => {
			const total = progress.total_matched || 0;
			const processed = Math.min(progress.processed, total || progress.processed);
			toast.loading("Recalculating log costs...", {
				id: toastId,
				description:
					total > 0
						? `${processed}/${total} checked, ${progress.updated} updated, ${progress.skipped} skipped`
						: "Finding logs with missing costs",
			});
		});

		toast.promise(recalculatePromise, {
			id: toastId,
			loading: "Recalculating log costs...",
			success: (response) => ({
				message: `Recalculated costs for ${response.updated} logs`,
				description: `${response.updated} logs updated, ${response.skipped} logs skipped, ${response.remaining} logs remaining`,
				duration: 5000,
			}),
			error: (err) => getErrorMessage(err),
		});

		try {
			await recalculatePromise;
			await fetchLogs();
			await fetchStats();
		} catch {}
	}, [filters, recalcMode, fetchLogs, fetchStats]);

	const handleSearchChange = useCallback(
		(value: string) => {
			setLocalSearch(value);
			if (searchTimeoutRef.current) clearTimeout(searchTimeoutRef.current);
			searchTimeoutRef.current = setTimeout(() => {
				onFiltersChange({ ...filtersRef.current, content_search: value });
			}, 500);
		},
		[onFiltersChange],
	);

	return (
		<div className="flex grow items-center justify-between space-x-2">
			<Button
				data-testid="logs-refresh-btn"
				variant="outline"
				size="sm"
				className="h-7.5 disabled:opacity-100"
				onClick={() => {
					fetchLogs();
					fetchStats();
					fetchHistogram();
				}}
				disabled={loading}
			>
				<RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
				Refresh
			</Button>
			<Button
				data-testid="logs-live-btn"
				variant={polling ? "default" : "outline"}
				size="sm"
				className="h-7.5"
				onClick={() => onPollToggle(!polling)}
			>
				{polling ? <Radio className="h-4 w-4 animate-pulse" /> : <Radio className="h-4 w-4" />}
				Live
			</Button>
			<div className="border-input flex h-7.5 flex-1 items-center gap-2 rounded-sm border">
				<Search className="mr-0.5 ml-2 size-4" />
				<Input
					type="text"
					className="!h-7 rounded-tl-none rounded-tr-sm rounded-br-sm rounded-bl-none border-none bg-slate-50 shadow-none outline-none focus-visible:ring-0"
					placeholder="Search logs"
					value={localSearch}
					onChange={(e) => handleSearchChange(e.target.value)}
				/>
			</div>

			<DateTimePickerWithRange
				triggerTestId="filter-date-range"
				dateTime={{ from: startTime, to: endTime }}
				predefinedPeriod={period || undefined}
				showTimezone
				timezone={timezone}
				onTimezoneChange={setTimezone}
				onDateTimeUpdate={(p) => {
					setStartTime(p.from);
					setEndTime(p.to);
					onPeriodChange(undefined, p.from, p.to);
				}}
				preDefinedPeriods={TIME_PERIODS}
				onPredefinedPeriodChange={(periodValue) => {
					if (!periodValue) return;
					const { from, to } = getRangeForPeriod(periodValue);
					setStartTime(from);
					setEndTime(to);
					// Relative period: store it in URL and update timestamps via parent
					onPeriodChange(periodValue, from, to);
				}}
			/>
			<Popover open={openMoreActionsPopover} onOpenChange={setOpenMoreActionsPopover}>
				<PopoverTrigger asChild>
					<Button variant="outline" size="sm" className="h-7.5 w-7.5">
						<MoreVertical className="h-4 w-4" />
					</Button>
				</PopoverTrigger>
				<PopoverContent className="bg-accent w-[250px] p-2" align="end">
					<Command>
						<CommandList>
							<CommandItem
								className="hover:bg-accent/50 cursor-pointer"
								onSelect={() => {
									setOpenMoreActionsPopover(false);
									setRecalcDialogOpen(true);
								}}
							>
								<Calculator className="text-muted-foreground size-4" />
								<div className="flex flex-col">
									<span className="text-sm">Recalculate costs</span>
									<span className="text-muted-foreground text-xs">Recompute cost for logs in this view</span>
								</div>
							</CommandItem>
						</CommandList>
					</Command>
				</PopoverContent>
			</Popover>
			<ColumnConfigDropdown
				entries={columnEntries}
				labels={columnLabels}
				onToggleVisibility={onToggleColumnVisibility}
				onReset={onResetColumns}
			/>

			<Dialog open={recalcDialogOpen} onOpenChange={setRecalcDialogOpen}>
				<DialogContent className="sm:max-w-[440px]">
					<DialogHeader>
						<DialogTitle>Recalculate costs</DialogTitle>
						<DialogDescription>
							The current time window and filters will be applied. Choose which logs to recompute cost for.
						</DialogDescription>
					</DialogHeader>

					<div className="flex flex-col gap-2 py-1">
						<RecalculateModeOption
							selected={recalcMode === "missing"}
							onSelect={() => setRecalcMode("missing")}
							title="Missing cost only"
							description="Only recompute logs that don't have a cost yet."
						/>
						<RecalculateModeOption
							selected={recalcMode === "all"}
							onSelect={() => setRecalcMode("all")}
							title="All selected logs"
							description="Recompute cost for every log matching the current filters."
						/>
					</div>

					<p className="text-muted-foreground text-xs">
						{recalcMode === "all" ? (
							<>
								<span className="text-foreground font-medium">{formatCompactNumber(totalLogs)}</span> logs match the current filters and will be
								recalculated.
							</>
						) : (
							"Logs in the current window that don't have a cost yet will be recalculated."
						)}
					</p>

					<DialogFooter>
						<Button variant="outline" size="sm" onClick={() => setRecalcDialogOpen(false)}>
							Cancel
						</Button>
						<Button size="sm" onClick={handleRecalculateCosts}>
							<Calculator className="size-4" />
							Recalculate
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</div>
	);
}

function RecalculateModeOption({
	selected,
	onSelect,
	title,
	description,
}: {
	selected: boolean;
	onSelect: () => void;
	title: string;
	description: string;
}) {
	return (
		<button
			type="button"
			onClick={onSelect}
			aria-pressed={selected}
			className={`flex items-start gap-3 rounded-md border p-3 text-left transition-colors ${
				selected ? "border-primary bg-primary/5" : "border-input hover:bg-accent/50"
			}`}
		>
			<span
				className={`mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border ${
					selected ? "border-primary bg-primary text-primary-foreground" : "border-muted-foreground/40"
				}`}
			>
				{selected && <Check className="size-3" />}
			</span>
			<span className="flex flex-col gap-0.5">
				<span className="text-sm font-medium">{title}</span>
				<span className="text-muted-foreground text-xs">{description}</span>
			</span>
		</button>
	);
}

async function recalculateCostsWithProgress(
	filters: LogFiltersType,
	missingCostOnly: boolean,
	onProgress: (progress: RecalculateCostProgress) => void,
): Promise<RecalculateCostResponse> {
	const headers: Record<string, string> = {
		Accept: "text/event-stream",
		"Content-Type": "application/json",
	};
	const tempToken = getActiveTempToken();
	if (tempToken) {
		headers["X-Bifrost-Temp-Token"] = tempToken;
	}

	const response = await fetch(`${getApiBaseUrl()}/logs/recalculate-cost`, {
		method: "POST",
		credentials: "include",
		headers,
		// Override the page's own missing_cost_only filter with the mode chosen in the dialog.
		body: JSON.stringify({ filters: { ...filters, missing_cost_only: missingCostOnly } }),
	});

	if (!response.ok) {
		throw await readRecalculateCostError(response);
	}
	if (!response.body) {
		throw new Error("Recalculate cost stream is unavailable");
	}

	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";
	let finalResult: RecalculateCostResponse | undefined;

	while (true) {
		const { value, done } = await reader.read();
		if (done) break;
		buffer += decoder.decode(value, { stream: true });
		const events = buffer.split("\n\n");
		buffer = events.pop() || "";
		for (const eventBlock of events) {
			const parsed = parseSSEEvent(eventBlock);
			if (!parsed || parsed.data === "[DONE]") continue;
			if (parsed.event === "error") {
				throw parseRecalculateCostStreamError(parsed.data);
			}
			if (parsed.event === "progress") {
				onProgress(JSON.parse(parsed.data) as RecalculateCostProgress);
				continue;
			}
			if (parsed.event === "done") {
				finalResult = JSON.parse(parsed.data) as RecalculateCostResponse;
			}
		}
	}

	buffer += decoder.decode();
	if (buffer.trim()) {
		const parsed = parseSSEEvent(buffer);
		if (parsed?.event === "error") {
			throw parseRecalculateCostStreamError(parsed.data);
		}
		if (parsed?.event === "done") {
			finalResult = JSON.parse(parsed.data) as RecalculateCostResponse;
		}
	}

	if (!finalResult) {
		throw new Error("Recalculate cost stream ended before a final result was received");
	}
	return finalResult;
}

function parseSSEEvent(block: string): { event: string; data: string } | undefined {
	let event = "message";
	const data: string[] = [];
	for (const rawLine of block.split("\n")) {
		const line = rawLine.trimEnd();
		if (line.startsWith("event: ")) {
			event = line.slice(7);
		} else if (line.startsWith("data: ")) {
			data.push(line.slice(6));
		}
	}
	if (data.length === 0) return undefined;
	return { event, data: data.join("\n") };
}

async function readRecalculateCostError(response: Response): Promise<Error> {
	try {
		return parseRecalculateCostStreamError(await response.text());
	} catch {
		return new Error(`Failed to recalculate costs (${response.status})`);
	}
}

function parseRecalculateCostStreamError(data: string): Error {
	try {
		const parsed = JSON.parse(data) as { error?: { message?: string }; message?: string };
		return new Error(parsed.error?.message || parsed.message || "Failed to recalculate costs");
	} catch {
		return new Error(data || "Failed to recalculate costs");
	}
}