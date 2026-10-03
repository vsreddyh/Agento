/**
 * Subagent spawning — `spawn_subagent`.
 *
 * Runs an independent agent to completion on a self-contained sub-task and returns
 * its answer. This is the "delegation" the Hermes profiles had: a second agent with
 * its own transcript, own tool use and own compaction, so the parent's context stays
 * small while the work still gets done.
 *
 * The subagent is a **separate `pi` process**, not an in-process SDK session. That is
 * the whole design, and it was chosen because of what a leak costs:
 *
 *  - An SDK session (`createAgentSession`) does not load the CLI's built-in
 *    extensions, so reproducing MCP means building a `DefaultResourceLoader` by hand.
 *    That works — 43 MCP tools, verified — but the child then owns five MCP server
 *    subprocesses that nothing ever reclaims. `AgentSession.dispose()` does not emit
 *    `session_shutdown`, which is the only event the MCP extension closes its
 *    connections on, and `AgentSessionRuntime.dispose()` (which does emit it) does not
 *    reproduce the MCP wiring. Measured: every delegation stranded five processes,
 *    accumulating for the life of the container.
 *  - A process is reaped by `RpcClient.stop()`, which SIGTERMs the CLI; the CLI closes
 *    its own MCP clients on the way out. Measured: zero processes left behind, across
 *    repeated delegations.
 *
 * So the properties below come from documented CLI flags on a disposable process
 * rather than from runtime checks inside a live one:
 *
 *  1. A subagent CANNOT spawn another. `--no-extensions` stops the child loading any
 *     extension, including this one, so the tool does not exist in its world. This is
 *     structural, not advisory: a depth counter is a limit a bug could step past, and
 *     two agents each spawning the other is an unbounded token loop — the one failure
 *     this migration exists to prevent.
 *
 *  2. The child keeps the parent's real capabilities. `--no-extensions` also disables
 *     the built-in extensions, so MCP, codemode and tool search are re-enabled
 *     explicitly with `-e builtin:mcp -e builtin:codemode -e builtin:tool-search`.
 *     Skipping that step would yield a second agent that cannot touch money,
 *     cookbook, health, tasks or projects — no delegation at all in practice. The
 *     parent's own `--skill` flags are forwarded too, so per-profile skills survive.
 *
 *  3. Nothing is left on disk. `--no-session` means no session file per delegation for
 *     the gateway's session store, which has never heard of these conversations, to
 *     accumulate forever.
 *
 *  4. The child's cost is folded into this tool's own `usage`, because Pi adds a
 *     nested result's usage to the caller's totals. Omitting it makes delegation look
 *     nearly free while being one of the most expensive things the agent can do — and
 *     this migration exists to make cost visible. The figure comes from
 *     `getSessionStats()` rather than by summing streamed frames: per-delta usage
 *     repeats within one message, so summing deltas multiplies a single turn's cost.
 */

import { existsSync, statSync } from "node:fs";
import { isAbsolute, resolve } from "node:path";
import { Type, type ThinkingLevel, type Usage } from "@earendil-works/pi-ai";
import { defineTool, RpcClient, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

/**
 * The tool's own name.
 *
 * Recursion is prevented by `--no-extensions` on the child, not by this constant —
 * the child loads no extensions, so this tool is never registered there. The
 * constant exists so `name` and the documentation cannot drift apart.
 */
const SELF = "spawn_subagent";

/**
 * Cap on what comes back to the parent.
 *
 * A subagent that wrote a long report must not be able to push the parent's context
 * to its limit — that is the opposite of why delegation exists. Truncation is stated
 * in the returned text so the parent re-asks for a specific part instead of assuming
 * it holds the whole answer.
 */
const MAX_RESULT_CHARS = 8000;

/**
 * Fail if `work` has not settled within `ms`.
 *
 * Startup is bounded for the same reason the prompt is: six processes are pinned from
 * the moment `start()` is called, and a child that stalls while connecting MCP would
 * otherwise sit there well past `timeoutSeconds` with nothing waiting on it. The
 * caller still tears the child down in its `finally`, so losing the race costs
 * nothing but the wait.
 */
function within<T>(ms: number, what: string, work: Promise<T>): Promise<T> {
	let timer: ReturnType<typeof setTimeout> | undefined;
	const deadline = new Promise<never>((_resolve, reject) => {
		timer = setTimeout(
			() => reject(new Error(`the subagent ${what} did not finish within ${ms / 1000}s`)),
			ms,
		);
	});
	return Promise.race([work, deadline]).finally(() => clearTimeout(timer)) as Promise<T>;
}

/**
 * What is left of the delegation's budget.
 *
 * Startup and the prompt share one ceiling rather than each getting the full
 * `timeoutMs`: the ceiling exists to bound how long six processes stay pinned, and
 * two phases each granted the whole number would let one call hold them for twice
 * the time the caller asked to be capped at.
 */
function remainingBudget(timeoutMs: number, startedAt: number, now = Date.now()): number {
	return timeoutMs - (now - startedAt);
}

/** A skill path is made absolute so it survives the child running elsewhere. */
function absolute(path: string, base: string): string {
	return isAbsolute(path) ? path : resolve(base, path);
}

/** Long enough for real work; short enough that a stuck child fails visibly. */
const DEFAULT_TIMEOUT_MS = 900_000;

/**
 * Cap on the task text.
 *
 * Output is truncated on the way back, so input is bounded for symmetry and because
 * a runaway task becomes a runaway child prompt. 20k characters is roughly 5k tokens
 * — generous for a delegation brief, and the error says what to do instead, since
 * the right move for a larger job is to name files rather than paste them.
 */
const MAX_TASK_CHARS = 20_000;

/**
 * Hard ceiling on a delegation, which is also the default.
 *
 * A subagent pins six processes (one CLI plus five MCP servers) for as long as it
 * runs, so the upper bound is a resource decision, not a preference. It is
 * deliberately one-sided: `timeoutSeconds` can only *shorten* a delegation, never
 * extend it past the ceiling. Work that cannot finish inside it should be split
 * across calls.
 */
const MAX_TIMEOUT_MS = 900_000;

/**
 * Ceiling on teardown, deliberately outside `MAX_TIMEOUT_MS`.
 *
 * `stop()` runs in a `finally` after the call has already failed or answered, so
 * there is no budget left to spend and nothing left waiting on it. Without its own
 * bound, a child stalling while closing its MCP servers would hold all six processes
 * indefinitely — the one leak this file is written to prevent.
 */
const TEARDOWN_TIMEOUT_MS = 30_000;

/**
 * Accepted thinking levels.
 *
 * `off` is not in Pi's `ThinkingLevel` union — it lives in `ModelThinkingLevel` —
 * but it is a value this system actually uses (every profile runs
 * `muse-spark-1.3-contributor:off`), and `setThinkingLevel` accepts it at runtime.
 * A whitelist built from the declared type alone would reject the common case.
 *
 * Validating at all is worth it because Pi **silently accepts an unknown level**:
 * a typo would otherwise leave the subagent at a depth nobody chose, with no error
 * anywhere.
 */
const THINKING_LEVELS = new Set(["off", "minimal", "low", "medium", "high", "xhigh", "max"]);

/** The slice of the extension context this tool actually depends on. */
type SpawnContext = {
	cwd: string;
	model?: { provider?: string; id?: string };
	thinkingLevel?: string;
	modelRegistry: { find(provider: string, modelId: string): unknown };
};

/** Events as delivered by `RpcClient.onEvent` / `promptAndWait`. */
type SessionEvent = {
	type?: string;
	assistantMessageEvent?: { type?: string; delta?: string };
	errorMessage?: string;
};

function truncate(text: string, limit = MAX_RESULT_CHARS): string {
	if (text.length <= limit) return text;
	return (
		text.slice(0, limit) +
		`\n\n[truncated: ${text.length - limit} more characters. ` +
		`Re-ask for the part you need rather than assuming this is everything.]`
	);
}

/**
 * Concatenate the streamed text deltas that make up the child's answer.
 *
 * Accumulation stops at twice `MAX_RESULT_CHARS` rather than building the whole
 * string: a verbose child would otherwise be truncated on the way out but still
 * allowed to grow the parent's memory without limit first. The overshoot is
 * deliberate — it leaves `truncate` enough text to report how much was actually
 * dropped, instead of a made-up number.
 *
 * The cap is applied to the *append*, not just the loop condition. A child that
 * emits one enormous delta would pass the check and overshoot by its whole size
 * in a single step, which is the exact case the cap exists for.
 */
function answerOf(events: readonly SessionEvent[]): string {
	const limit = MAX_RESULT_CHARS * 2;
	let answer = "";
	for (const event of events) {
		if (answer.length >= limit) break;
		if (
			event.type !== "message_update" ||
			event.assistantMessageEvent?.type !== "text_delta"
		) {
			continue;
		}
		answer += (event.assistantMessageEvent.delta ?? "").slice(0, limit - answer.length);
	}
	return answer;
}

/**
 * Event types seen that contributed no text.
 *
 * Only used in the failure path. If Pi ever changes the shape that carries the
 * answer, the child will have answered perfectly and this tool will report "no
 * text" — so the error says what it did see, which is the difference between a
 * five-second fix and a guessing game.
 */
function unrecognised(events: readonly SessionEvent[]): string {
	const ignored = new Set<string>();
	for (const event of events) {
		// Top-level types are collected too, not skipped: if Pi ever renames or
		// replaces the `message_update` envelope, skipping non-matching events here
		// would report "Events seen:" with nothing listed — precisely the blindness
		// this function exists to remove. Distinct types are few, so the list stays
		// short even though a single turn emits many events.
		ignored.add(event.type ?? "(no type)");
		const kind = event.assistantMessageEvent?.type;
		if (kind) ignored.add(kind);
	}
	// The two shapes this function already understands: the envelope every text
	// delta arrives in, and the delta itself. Reporting them would bury the signal.
	// A *renamed* envelope still shows up, because its new name is not on this list.
	ignored.delete("message_update");
	ignored.delete("text_delta");
	const names = [...ignored].sort();
	return names.length ? ` Events seen: ${names.join(", ")}.` : "";
}

/**
 * A timeout the model controls, bounded at both ends.
 *
 * Rejected rather than clamped at the low end: `0` and negatives are nonsense, and
 * silently turning `0` into the 15-minute default hides the mistake. The floor is 1s
 * because anything below that still pays the full cost — a process and five MCP
 * servers — to time out almost immediately.
 */
function resolveTimeout(seconds: number | undefined): number {
	if (seconds === undefined) return DEFAULT_TIMEOUT_MS;
	if (!Number.isFinite(seconds) || seconds < 1) {
		throw new Error(
			// `String`, not `JSON.stringify`: the whole point of this branch is a value
			// that is not a usable number, and stringify reports `NaN` as `null` — so a
			// caller who passed NaN would be told they passed null.
			`timeoutSeconds must be a number of seconds of at least 1, got ${String(seconds)}`,
		);
	}
	return Math.min(seconds * 1000, MAX_TIMEOUT_MS);
}

/**
 * Reject a thinking level Pi would silently ignore.
 *
 * Only an **explicitly requested** level is checked. The inherited level is passed
 * through untouched: it came from a working parent session, and validating it against
 * a hardcoded list would mean that if Pi adds a level, delegation breaks for every
 * profile using it — through a path where the caller passed nothing at all.
 */
function resolveThinking(requested: unknown, inherited?: string): string | undefined {
	if (requested === undefined) return inherited;
	// Typed as `unknown` because the schema is not the only caller: a `typeof` guard
	// turns a wrong-typed value into the message that names the field, where `.trim()`
	// on a number would throw a `TypeError` that names nothing.
	if (typeof requested !== "string") {
		throw new Error(`thinking must be a string, got ${typeof requested}`);
	}
	if (!THINKING_LEVELS.has(requested.trim())) {
		throw new Error(
			`unknown thinking level ${JSON.stringify(requested)}; expected one of ` +
				`${[...THINKING_LEVELS].join(", ")}`,
		);
	}
	return requested.trim();
}

/**
 * Check the working directory before spawning.
 *
 * A relative path is resolved against **this agent's** directory, not left for the
 * child to resolve against the `pi` process cwd. Those are the same today, but they
 * are not the same by contract, and a bare `src/foo` silently landing somewhere
 * else — with no error — is the worst possible outcome for a path.
 *
 * This is a typo guard, not a security boundary: the caller already holds `bash` and
 * full filesystem access, so `cwd` grants nothing it did not have. What it does
 * prevent is an unreadable or misspelled path turning into a child that dies during
 * startup with an error that points nowhere near the cause.
 */
function resolveCwd(requested: unknown, base: string): string {
	// Same rule as task and model: an empty string is a typo, not a request to
	// inherit. `requested ?? base` would quietly turn `""` into `base`. A non-string is
	// a typo too, and is reported as one rather than as a `TypeError` from `.trim()`.
	if (requested !== undefined && typeof requested !== "string") {
		throw new Error(`cwd must be a string, got ${typeof requested}`);
	}
	if (requested !== undefined && !requested.trim()) {
		throw new Error("cwd must not be empty; omit it to inherit this agent's");
	}
	const wanted = requested ?? base;
	const dir = isAbsolute(wanted) ? wanted : resolve(base, wanted);
	// Checked as one unit: a separate existence check then a stat is a TOCTOU window
	// in which the directory can disappear, and the raw ENOENT from `statSync` would
	// then point at the wrong cause.
	try {
		if (!statSync(dir).isDirectory()) {
			throw new Error(`not a directory: ${dir}`);
		}
	} catch (error) {
		if (error instanceof Error && error.message.startsWith("not a directory:")) throw error;
		// Only `ENOENT`/`ENOTDIR` mean "missing". Everything else — `EACCES`, `EPERM`,
		// `ELOOP` — means the path is there and something else is wrong, and reporting
		// those as "no such directory" sends the caller to fix a path that exists.
		const code = (error as NodeJS.ErrnoException | undefined)?.code;
		if (code === "ENOENT" || code === "ENOTDIR") {
			throw new Error(`no such directory: ${dir}`);
		}
		throw new Error(`cannot use cwd ${dir}: ${code ?? (error as Error)?.message ?? "unknown error"}`);
	}
	return dir;
}

/**
 * Reject a task with nothing in it.
 *
 * An empty prompt would otherwise start a process, connect five MCP servers, and
 * then return nothing — the most expensive way to do nothing.
 *
 * Takes `unknown` deliberately: `task` is required in the schema, but depending on
 * the caller having validated it first would turn a missing or wrong-typed argument
 * into a `TypeError` from `.trim()` instead of the message that says what to fix.
 */
function resolveTask(requested: unknown): string {
	if (requested !== undefined && typeof requested !== "string") {
		throw new Error(`task must be a string, got ${typeof requested}`);
	}
	const task = requested?.trim();
	if (!task) throw new Error("task must not be empty");
	if (task.length > MAX_TASK_CHARS) {
		throw new Error(
			`task is ${task.length} characters, over the ${MAX_TASK_CHARS} limit. ` +
				`Name the files to read instead of pasting their contents.`,
		);
	}
	return task;
}

/**
 * The CLI to launch.
 *
 * `RpcClient` defaults to a relative `dist/cli.js` and runs it as `node <path>`, which
 * resolves against the profile directory and does not exist. This process *is* that
 * CLI, so its own entry point is the correct answer and needs no configuration.
 */
function cliPath(): string {
	const entry = process.argv[1];
	// Absolute, because the child can run in a different cwd: a relative entry would
	// satisfy `existsSync` here and then resolve against the wrong directory there.
	if (entry && existsSync(entry)) return absolute(entry, process.cwd());
	throw new Error(
		`cannot locate the pi CLI to launch a subagent (process.argv[1] is ${JSON.stringify(entry)}). ` +
			`This tool must run inside the pi CLI.`,
	);
}

/**
 * The `--skill` paths this agent was started with, so the child gets them too.
 *
 * Only `--skill <path>` and `--skill=<path>` are recognised. A skill the parent loaded
 * some other way is not forwarded; that is a narrower gap than losing every skill,
 * and guessing at other spellings risks passing a flag the CLI does not take.
 */
function parentSkillArgs(argv: readonly string[], base: string): string[] {
	const forwarded: string[] = [];
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i];
		if (arg === "--skill" && argv[i + 1] !== undefined) {
			// A value beginning with `-` is the next flag, not a path. Forwarding it as
			// one would hand the child `--no-session` as a skill directory and fail at
			// startup with an error about something else entirely.
			if (argv[i + 1].startsWith("-")) continue;
			forwarded.push("--skill", absolute(argv[i + 1], base));
			i++;
		} else if (arg?.startsWith("--skill=")) {
			// An empty value is not a path. Forwarding `--skill=` would have the child
			// fail at startup over something the parent never actually specified.
			const value = arg.slice("--skill=".length);
			if (value.trim()) forwarded.push(`--skill=${absolute(value, base)}`);
		}
	}
	return forwarded;
}

/**
 * Split a model id into the provider and bare model the CLI takes separately.
 *
 * A bare id is looked up under the parent's own provider, which is what a caller
 * means by "use the fast one". `createAgentSession`-style lazy resolution does not
 * apply here — the CLI validates at startup — but an unknown model still has to be
 * refused here, where the parent can see what was asked for, rather than as a failed
 * turn inside a subagent.
 */
function resolveModel(
	ctx: SpawnContext,
	requested: unknown,
): { provider: string; model: string } {
	const parent = ctx.model;
	if (requested !== undefined && typeof requested !== "string") {
		throw new Error(`model must be a string, got ${typeof requested}`);
	}
	// Trimmed first: untrimmed whitespace would fail the registry lookup for a model
	// that does exist, with an error naming a model the caller never asked for.
	const wanted = requested?.trim();
	// `undefined` means "inherit"; an empty string is a typo and must not quietly
	// become an inheritance the caller did not ask for.
	if (requested !== undefined && !wanted) {
		throw new Error("model must not be empty; omit it to inherit this agent's model");
	}
	if (wanted === undefined) {
		if (!parent?.provider || !parent.id) {
			throw new Error("this agent has no current model to inherit; pass model explicitly");
		}
		const prefix = `${parent.provider}/`;
		return {
			provider: parent.provider,
			model: parent.id.startsWith(prefix) ? parent.id.slice(prefix.length) : parent.id,
		};
	}

	const slash = wanted.indexOf("/");
	const provider = slash >= 0 ? wanted.slice(0, slash) : parent?.provider;
	const model = slash >= 0 ? wanted.slice(slash + 1) : wanted;
	if (!provider) {
		throw new Error(
			`cannot resolve ${JSON.stringify(wanted)}: no provider given and this agent has ` +
				`no current model to infer one from. Use "provider/model".`,
		);
	}
	// `provider/` with nothing after it would otherwise reach the registry as an empty
	// model id and be reported as an unknown model rather than as a typo.
	if (!model) {
		throw new Error(
			`model ${JSON.stringify(wanted)} names a provider but no model; ` +
				`expected "provider/model" or a bare model id.`,
		);
	}
	// The registry is third-party: if `find` throws, that is not something the parent
	// can act on, so it is reported as the unknown model it almost certainly means.
	let found: unknown;
	try {
		found = ctx.modelRegistry.find(provider, model);
	} catch {
		found = undefined;
	}
	// Falsy, not `=== undefined`: `find` is third-party and a `null` for "not found"
	// is as plausible as `undefined`. Passing `null` through would hand the child a
	// model that does not exist and fail on its first turn.
	if (!found) {
		throw new Error(
			`unknown model ${JSON.stringify(wanted)} (looked up ${provider}/${model}). ` +
				`Omit "model" to inherit this agent's, or check /api/model/options for the list.`,
		);
	}
	return { provider, model };
}

/**
 * Coerce a stat to a usable number.
 *
 * `?? 0` is not enough: it passes `NaN` and `"5"` straight through, and these
 * values are reported to the parent as turn and tool-call counts. Negatives are
 * clamped for the same reason: a stat can only be a count, and a negative one
 * would subtract from every total it is added to.
 */
function count(value: unknown): number {
	if (typeof value !== "number" || !Number.isFinite(value)) return 0;
	return Math.max(0, value);
}

/**
 * Map session stats onto the `Usage` shape Pi folds into caller totals.
 *
 * Every field goes through `count()`. A missing or non-numeric stat would otherwise
 * land in the totals as `NaN` and stay there: `addUsageToTotals` only ever adds, so
 * one bad turn poisons every total after it for the rest of the session, silently.
 */
function usageOf(
	tokens:
		| {
				input: number;
				output: number;
				cacheRead: number;
				cacheWrite: number;
				total: number;
		  }
		| undefined,
	cost: number | undefined,
): Usage {
	return {
		input: count(tokens?.input),
		output: count(tokens?.output),
		cacheRead: count(tokens?.cacheRead),
		cacheWrite: count(tokens?.cacheWrite),
		totalTokens: count(tokens?.total),
		// `addUsageToTotals` reads usage.cost.total with no guard, so this object has
		// to exist even though stats only break the cost out as a single number.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: count(cost) },
	};
}

/** The tail of the child's stderr, for when it fails without saying why. */
function stderrTail(text: string, limit = 500): string {
	const trimmed = text.trim();
	return trimmed.length <= limit ? trimmed : trimmed.slice(-limit);
}

const spawnSubagent = defineTool({
	name: SELF,
	label: "Spawn subagent",
	description:
		"Run an independent agent to completion on a self-contained sub-task and return " +
		"its answer. The subagent has its own context, the same tools and the same MCP " +
		"servers as you, so give it a task that stands alone: it cannot see this " +
		"conversation and cannot ask you anything. Use it for work too bulky to do " +
		"inline — reading across many files, summarising a subtree, checking a " +
		"hypothesis end to end. Do not use it to split a task whose steps depend on " +
		"each other, and do not use it to avoid work you can simply do: every call " +
		"starts a second agent, which costs real tokens. A subagent cannot spawn " +
		"another. Call these one at a time — each one holds a pi process and five " +
		"MCP servers for as long as it runs, so several at once multiplies both.",
	parameters: Type.Object({
		task: Type.String({
			description:
				"What the subagent should do, stated so it makes sense with no other " +
				"context. Include the paths, names or values it needs.",
		}),
		cwd: Type.Optional(
			Type.String({
				description: "Directory for the subagent to work in. Defaults to this agent's own.",
			}),
		),
		model: Type.Optional(
			Type.String({
				description:
					'Model id, either "provider/model" or a bare id resolved under this ' +
					"agent's provider. Defaults to this agent's model.",
			}),
		),
		thinking: Type.Optional(
			Type.String({
				description:
					"Thinking level: off, minimal, low, medium, high, xhigh or max. " +
					"Defaults to this agent's.",
			}),
		),
		timeoutSeconds: Type.Optional(
			Type.Number({
				description:
					`Give up after this many seconds (max ${MAX_TIMEOUT_MS / 1000}, ` +
					`default ${DEFAULT_TIMEOUT_MS / 1000}), plus up to 30s to tear the ` +
					`child down afterwards. Work needing longer should be split.`,
			}),
		),
	}),

	async execute(_toolCallId, params, signal, _onUpdate, ctx) {
		const started = Date.now();

		// Everything the model controls is checked here, before a process exists, so
		// a bad value is refused where the parent can see what it asked for instead of
		// as a child that dies during startup.
		const { provider, model } = resolveModel(ctx, params.model);
		const task = resolveTask(params.task);
		const cwd = resolveCwd(params.cwd, ctx.cwd);
		const timeoutMs = resolveTimeout(params.timeoutSeconds);
		const thinking = resolveThinking(params.thinking, ctx.thinkingLevel);

		const client = new RpcClient({
			cliPath: cliPath(),
			cwd,
			provider,
			model,
			args: [
				// Property 1 above.
				"--no-extensions",
				// Property 2 above — re-enable what --no-extensions switched off.
				"-e",
				"builtin:mcp",
				"-e",
				"builtin:codemode",
				"-e",
				"builtin:tool-search",
				// Property 3 above.
				"--no-session",
				// Property 2 above — keep the parent's per-profile skills.
				...parentSkillArgs(process.argv, ctx.cwd),
			],
		});

		// Cancelling the parent turn has to reach the child, or it runs to completion
		// against a session nobody is listening to.
		const onAbort = () => {
			// Swallowed for the same reason as `stop()` below: this runs on the parent's
			// signal, where there is nobody left to receive a rejection, and an
			// unhandled one would surface as an unrelated crash.
			void client.abort().catch(() => {});
		};
		signal?.addEventListener("abort", onAbort, { once: true });

		try {
			await within(timeoutMs, "startup", client.start());

			// The listener above only fires on a *later* abort. If the parent turn was
			// already cancelled by the time we got here, that never happens and the
			// child would run to completion against a session nobody is listening to.
			if (signal?.aborted) {
				await client.abort().catch(() => {});
				throw new Error("cancelled before the subagent was prompted");
			}

			// The cast is deliberate and load-bearing: `setThinkingLevel` is typed
				// `ThinkingLevel`, which does not contain `off` — that is a
				// `ModelThinkingLevel` — yet the RPC method accepts it at runtime, and
				// `off` is what every profile here runs. The cast keeps compiling if
				// upstream narrows the union, so the real risk is a runtime refusal,
				// which surfaces as this call's error rather than silently.
				if (thinking) await client.setThinkingLevel(thinking as ThinkingLevel);

			// The prompt gets the remainder, so the whole call stays inside the ceiling.
			const remaining = remainingBudget(timeoutMs, started);
			if (remaining <= 0) {
				throw new Error(`the subagent used its whole ${timeoutMs / 1000}s budget starting up`);
			}
			const events = (await client.promptAndWait(task, undefined, remaining)) as SessionEvent[];
			const answer = answerOf(events);

			if (!answer.trim()) {
				// An empty string reads as "the subagent worked and concluded nothing",
				// which is a different claim from "the subagent failed". stderr and the
				// event types are the only things that separate them.
				const noise = stderrTail(client.getStderr());
				throw new Error(
					"the subagent finished without producing any text" +
						(noise ? `: ${noise}` : "") +
						". Say what it should report back." +
						unrecognised(events),
				);
			}

			// Property 4 above, best-effort.
			//
			// The answer is the deliverable; the stats are metadata on it. Fetching
			// them after the run means a failure here would throw away work the parent
			// has already paid six processes for, and the natural response — retry —
			// would pay that cost again. So a failure is recorded, not raised.
			let stats: Awaited<ReturnType<typeof client.getSessionStats>> | undefined;
			try {
				stats = await client.getSessionStats();
			} catch {
				stats = undefined;
			}
			const usage = usageOf(stats?.tokens, stats?.cost);

			return {
				content: [{ type: "text" as const, text: truncate(answer) }],
				details: {
					model: `${provider}/${model}`,
					thinking: thinking ?? "inherited",
					cwd,
					turns: count(stats?.assistantMessages),
					toolCalls: count(stats?.toolCalls),
					durationMs: Date.now() - started,
					// Says plainly that the figure below is absent rather than zero, so a
					// parent watching its own cost cannot mistake "unknown" for "free".
					usageUnavailable: stats === undefined,
					usage,
				},
				usage,
			};
		} finally {
			signal?.removeEventListener("abort", onAbort);
			// Reclaims the child and, through it, the MCP servers it started.
			//
			// Swallowed deliberately: `stop()` early-returns when the client never
			// started, so the realistic hazard is not that it throws but that a
			// teardown failure would replace the error that actually explains what
			// went wrong. The original is always the more useful one.
			//
			// Bounded, because teardown is the one wait with no budget behind it: the
			// startup and prompt deadlines were both inside `timeoutMs`, but a child
			// stalling while closing its MCP servers would otherwise pin all six
			// processes indefinitely, with nothing left waiting on them. Thirty
			// seconds is far longer than teardown has ever taken and far shorter than
			// the ceiling.
			await within(TEARDOWN_TIMEOUT_MS, "teardown", client.stop()).catch((error: unknown) => {
				// Not swallowed silently: this is the one path where the processes may
				// genuinely be left behind, and a leak with no trace is what makes the
				// next occurrence undebuggable.
				console.error(`spawn_subagent: teardown failed, the child may still be running: ${error}`);
			});
		}
	},
});

export default function (pi: ExtensionAPI) {
	pi.registerTool(spawnSubagent);
}