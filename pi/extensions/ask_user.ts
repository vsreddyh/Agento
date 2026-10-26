/**
 * Ask the user a question — `ask_user`.
 *
 * The agent calls this when it cannot proceed without a human decision:
 * which account, whether to continue, what to name something. The question
 * appears as an inline card in the app (options as buttons, or a free-text
 * field), and the tool call blocks until the user answers or cancels.
 *
 * Transport is Pi's extension UI sub-protocol, relayed by pi-gateway:
 * `ctx.ui.select` / `ctx.ui.input` emit `extension_ui_request` on stdout
 * and block until the gateway writes `extension_ui_response` with the
 * matching id on stdin. No gateway change was needed to load this — like
 * `subagent.ts`, it lives in `pi/extensions/` and every profile loads it.
 *
 * Decisions inherited from the issue, not re-litigated here:
 * - Inline card, not a notification or an approvals-sheet reuse.
 * - The turn blocks while waiting, with no question-level timeout (the
 *   turn's own budget is the bound; the profile lock is held meanwhile).
 * - No `confirm` surface: two options cover yes/no.
 * - `cancelled` (user dismissed, app killed mid-turn, turn ended) is a
 *   tool error, not a silent default — the model proceeds or re-asks.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

/** The tool's own name, kept in one place like `spawn_subagent`'s SELF. */
const SELF = "ask_user";

/** A question is a prompt, not a document. Long enough for context, short
 * enough to fit a card; the error says the limit so the model trims. */
const MAX_QUESTION_CHARS = 500;

/** 2–8 options: fewer is not a choice, more does not fit a card. */
const MIN_OPTIONS = 2;
const MAX_OPTIONS = 8;

/** One option is a button label, not a paragraph. */
const MAX_OPTION_CHARS = 100;

/** The whole card must stay small enough to read at a glance. */
const MAX_TOTAL_CHARS = 1000;

function resolveQuestion(requested: unknown): string {
	if (typeof requested !== "string") {
		throw new Error(`question must be a string, got ${typeof requested}`);
	}
	const question = requested.trim();
	if (!question) throw new Error("question must not be empty");
	if (question.length > MAX_QUESTION_CHARS) {
		throw new Error(
			`question is ${question.length} characters, over the ${MAX_QUESTION_CHARS} limit. ` +
				`Ask one short question instead.`,
		);
	}
	return question;
}

function resolveOptions(requested: unknown, question: string): string[] | undefined {
	if (requested === undefined) return undefined;
	// Typed as `unknown` because the schema is not the only caller: a
	// wrong-typed value must name the field, not throw a `TypeError`.
	if (!Array.isArray(requested)) {
		throw new Error(`options must be an array of strings, got ${typeof requested}`);
	}
	const options = requested.map((o, i) => {
		if (typeof o !== "string" || !o.trim()) {
			throw new Error(`options[${i}] must be a non-empty string`);
		}
		if (o.trim().length > MAX_OPTION_CHARS) {
			throw new Error(
				`options[${i}] is ${o.trim().length} characters, over the ${MAX_OPTION_CHARS} limit. ` +
					`Keep button labels short.`,
			);
		}
		return o.trim();
	});
	if (options.length < MIN_OPTIONS) {
		throw new Error(
			`options has ${options.length} entr${options.length === 1 ? "y" : "ies"}; ` +
				`give ${MIN_OPTIONS}–${MAX_OPTIONS}, or omit options for a free-text answer.`,
		);
	}
	if (options.length > MAX_OPTIONS) {
		throw new Error(
			`options has ${options.length} entries, over the ${MAX_OPTIONS} limit. ` +
				`Offer the most likely few instead.`,
		);
	}
	const seen = new Set(options.map((o) => o.toLowerCase()));
	if (seen.size !== options.length) {
		throw new Error("options must not contain duplicates");
	}
	const total = question.length + options.reduce((n, o) => n + o.length, 0);
	if (total > MAX_TOTAL_CHARS) {
		throw new Error(
			`question plus options is ${total} characters, over the ${MAX_TOTAL_CHARS} limit. ` +
				`Shorten the question or the options.`,
		);
	}
	return options;
}

const askUser = defineTool({
	name: SELF,
	label: "Ask user",
	description:
		"Ask the user a question and wait for their answer. Use it when you cannot " +
		"proceed without a human decision — which of two things they meant, whether " +
		"to continue, what to name something. The question appears as a card in " +
		"their app with your options as buttons (or a text field when you give no " +
		"options), and this call waits until they answer. Do not use it to report " +
		"progress or to ask something you could decide yourself: every call stalls " +
		"the turn on a human. If they dismiss the question instead of answering, " +
		"this fails — proceed with your best guess or ask differently.",
	parameters: Type.Object({
		question: Type.String({
			description: "The question, as it appears on the card.",
		}),
		options: Type.Optional(
			Type.Array(Type.String(), {
				description:
					"2–8 short button labels. Omit for a free-text answer field.",
			}),
		),
	}),

	async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
		// Everything the model controls is checked here, before Pi blocks on
		// a human: a bad value is refused where the parent can see what it
		// asked for, instead of as a card that never renders (or renders wrong).
		const question = resolveQuestion(params.question);
		const options = resolveOptions(params.options, question);

		// The relay answers with the request's id; a dismissal (or a turn
		// that ends while waiting) resolves to undefined, which is a tool
		// error — never a silent default the model might mistake for consent.
		const answer = options
			? await ctx.ui.select(question, options)
			: await ctx.ui.input(question);
		// A dismissal (or a turn that ends while waiting) resolves to
		// undefined — and so does an empty/blank answer, which carries no
		// decision: both are tool errors, never a silent default the model
		// might mistake for consent.
		if (answer === undefined || !answer.trim()) {
			throw new Error("The user did not answer.");
		}
		return {
			content: [{ type: "text" as const, text: answer.trim() }],
		};
	},
});

export default function (pi: ExtensionAPI) {
	pi.registerTool(askUser);
}
