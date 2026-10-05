package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.net.URLEncoder
import java.util.Locale
import java.util.concurrent.TimeUnit

private val JSON_MEDIA = "application/json; charset=utf-8".toMediaType()

/** One of the user's own tasks from the shared `tasks` collection,
 * served by health-api (`/api/tasks`). Openness is `completedAt` empty
 * (the store has no status field). */
data class ServerTask(
    val id: String,
    val name: String = "",
    val description: String = "",
    val dueDate: String = "",
    val dueTime: String = "",
    val estimatedMinutes: Int = 0,
    // Recurrence: either structured (every N unit) or custom (the user's
    // own words in repeatRule). 0/""/false everywhere = one-shot.
    val repeatEvery: Int = 0,
    val repeatUnit: String = "",
    val repeatCustom: Boolean = false,
    val repeatRule: String = "",
    val parallelable: Boolean = false,
    // Optimistic-concurrency guard: bumped by every server-side mutation.
    // Sent back on update so a stale editor is rejected, not overwritten.
    val revision: Int = 0,
    val completedAt: String = "",
    val createdAt: String = "",
    /**
     * The next occurrence the server created by itself when a structured
     * cadence was completed ("" when there is none, or when the repeat is a
     * custom condition the caller has to schedule itself).
     */
    val nextDueDate: String = "",
)

/** A bounded list response: the rows, and whether more exist. */
data class TaskList(
    val tasks: List<ServerTask>,
    val truncated: Boolean = false,
)

/** The deadline as an instant, or null when it cannot be parsed. */
fun ServerTask.dueMillisOrNull(): Long? = dueMillisOrNull(dueDate, dueTime)

/**
 * The one definition of when a task starts: [estimatedMinutes] before it is
 * due, or **null** when the task has no start of its own.
 *
 * A zero estimate means there is nothing to start early for, and that null
 * is why the two callers want different things out of this function — which
 * is exactly why neither of them should be deciding it for itself. The list
 * needs a moment to sort on and a day to group under, so
 * [startMillisOrNull] falls back to the deadline; the reminder engine must
 * not arm a "Start now" alert at all. One rule, one place, two answers,
 * rather than the same arithmetic written twice and held in step by a
 * comment in each file (#164).
 */
internal fun startMomentMillis(dueMillis: Long, estimatedMinutes: Int): Long? =
    if (estimatedMinutes > 0) dueMillis - estimatedMinutes * 60_000L else null

/**
 * The moment the work is meant to begin — which is exactly when the
 * "Start now" reminder fires, because both come from [startMomentMillis].
 * A task with no estimate of its own starts when it is due. Null only when
 * the task has no usable due time.
 */
fun ServerTask.startMillisOrNull(): Long? {
    val due = dueMillisOrNull() ?: return null
    return startMomentMillis(due, estimatedMinutes) ?: due
}

/** Start moment as the (date, HH:mm) pair the display helpers take. */
fun ServerTask.startParts(): Pair<String, String>? {
    val at = startMillisOrNull() ?: return null
    val ldt = java.time.Instant.ofEpochMilli(at).atZone(IST).toLocalDateTime()
    return ldt.toLocalDate().toString() to
        String.format(Locale.ROOT, "%02d:%02d", ldt.hour, ldt.minute)
}

/**
 * "Today, 07:00 → 09:00" — the start and the deadline on one line, **always
 * anchored on the start's day**.
 *
 * A task that crosses midnight (due 00:30, one-hour estimate) is grouped
 * under the day it *starts* on, so the line says the same thing the group
 * does: "Today, 23:30 → 00:30". Restating the deadline's own day here
 * ("Yesterday, 23:30 → Today, 00:30") would contradict the header the row
 * is sitting under, and the deadline's day is still spelled out in the
 * detail sheet, which labels it "Due" outright.
 *
 * The due time is formatted from the parsed instant rather than echoed from
 * the stored string, so both sides of the arrow are padded the same way.
 */
fun ServerTask.startToDueLine(today: java.time.LocalDate): String {
    val parts = startParts() ?: return friendlyDue(dueDate, dueTime, today)
    val start = friendlyDue(parts.first, parts.second, today)
    val at = dueMillisOrNull() ?: return start
    val d = java.time.Instant.ofEpochMilli(at).atZone(IST).toLocalDateTime()
    val time = String.format(Locale.ROOT, "%02d:%02d", d.hour, d.minute)
    if (start.isEmpty()) return time
    return "$start → $time"
}

/** True when the task repeats at all: a structured cadence or a custom
 * condition. One-shot tasks must not open a recreate draft. */
val ServerTask.hasRepeat: Boolean
    get() = repeatCustom || repeatEvery > 0 || repeatRule.isNotEmpty()

/** Human repeat line, matching the server's rendering: "Every 3 days",
 * the custom words verbatim, or "" for a one-shot task. */
fun ServerTask.repeatLabel(): String =
    repeatLabel(repeatEvery, repeatUnit, repeatCustom, repeatRule)

/** The same rendering for any source of the four fields. */
fun repeatLabel(every: Int, unit: String, custom: Boolean, text: String): String {
    // every <= 0 rather than == 0: a stray or half-migrated doc must never
    // render "Every day" from a zero count.
    if (custom || every <= 0 || unit.isEmpty()) return text
    val singular = if (unit.endsWith("s")) unit.dropLast(1) else unit
    return if (every <= 1) "Every $singular" else "Every $every $unit"
}

/** True while the task is still open (never completed). */
fun ServerTask.isOpen(): Boolean = completedAt.isBlank()

/**
 * Client for the user's tasks (same bearer auth as chat/sync, served
 * through the proxy's /api/ route — no profile prefix; the collection is
 * global, not per-assistant). Full CRUD against the same store and
 * validation the agent uses over MCP. Parsing is lenient across shapes;
 * rows without an id or name are skipped, missing fields blank.
 */
class TasksApi(context: Context) {

    companion object {
        /**
         * Builds the "what the app asked the server to store" record from write arguments.
         *
         * ONE builder for `create` and `update`, and `internal` so the tests call the real
         * thing rather than reimplementing it.
         *
         * That last part is the point. `UpdateContractWiringTest` originally hand-wrote its
         * own copy of this construction, which tested a COPY: if production drifted — an
         * untrimmed value, a field left null that should be asserted — every test stayed
         * green while the wiring rotted. It is the fixture-duplication trap one level up
         * from the one this PR already fixed for `TaskContractSent` itself, and it was mine
         * to walk into immediately after fixing the original.
         *
         * On the companion, not the instance: `TasksApi` needs a `Context`, so an instance
         * member is not callable from a plain JVM unit test — which is exactly where this
         * has to be callable from to be worth anything.
         *
         * Null in means null out: `update` passes only what it sent, and those stay null so
         * `contractMismatches` skips them. Strings are trimmed HERE, once, so the app sends
         * exactly what it asserts.
         */
        internal fun buildTaskContractSent(
            name: String? = null,
            description: String? = null,
            dueDate: String? = null,
            dueTime: String? = null,
            estimatedMinutes: Int? = null,
            repeatEvery: Int? = null,
            repeatUnit: String? = null,
            repeatCustom: Boolean? = null,
            repeatRule: String? = null,
            parallelable: Boolean? = null,
        ): TaskContractSent = TaskContractSent(
            name = name?.trim(),
            description = description?.trim(),
            dueDate = dueDate?.trim(),
            dueTime = dueTime?.trim(),
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit?.trim(),
            repeatCustom = repeatCustom,
            repeatRule = repeatRule?.trim(),
            parallelable = parallelable,
        )

    private fun checkContract(task: ServerTask, sent: TaskContractSent) {
        val mismatched = contractMismatches(sent, task)
        if (mismatched.isEmpty()) return
        // Not thrown. The write succeeded — the task IS stored — so failing it would
        // tell the user their work was lost when it was not, and they would enter it
        // again. Reporting an outcome that did not happen is the exact class of lie
        // #214 is about, so this warns and leaves the stored task authoritative.
        ContractWarnings.report(mismatched)
    }
    private fun parseOne(body: String): ServerTask =
        parseTask(JSONObject(body))
            ?: throw RuntimeException("Unexpected response shape")

    /** Ids are hex today, but encoded defensively so a malformed id can
     * never break the request path (the `+` fix-up mirrors ServerApi:
     * form-encoding emits `+` for space, valid only in query strings). */
    private fun encodeId(id: String): String =
        URLEncoder.encode(id.trim(), Charsets.UTF_8.name()).replace("+", "%20")

    private fun call(method: String, url: String, body: JSONObject? = null): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException("Server URL not configured"))
        }
        return runCatching {
            val builder = Request.Builder()
                .url(base + url)
                .header("Authorization", "Bearer ${password()}")
            when (method) {
                "GET" -> builder.get()
                "POST" -> builder.post((body?.toString() ?: "{}").toRequestBody(JSON_MEDIA))
                "PATCH" -> builder.patch((body?.toString() ?: "{}").toRequestBody(JSON_MEDIA))
                "DELETE" -> builder.delete()
                else -> throw IllegalArgumentException("Unknown method: $method")
            }
            http.newCall(builder.build()).execute().use { response ->
                val text = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    // A 404 on an /api/* route almost always means the app's
                    // Server URL points at the gateway (:8642) instead of the
                    // proxy (:8080) — the gateway serves chat only, while
                    // /api/* lives on health-api behind the proxy. Say so
                    // instead of surfacing a bare "Not Found".
                    if (response.code == 404 && url.startsWith("/api/")) {
                        throw RuntimeException(
                            "HTTP 404: ${text.take(200)} (is the Server URL " +
                                "the :8080 proxy? Direct :8642 serves chat only)"
                        )
                    }
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }
}

/** Pulls the server's `{"detail": "..."}` message out of an HTTP error so
 * validation failures (bad due date, unknown id) read as plain sentences.
 * Unescaping runs through org.json itself, so `\n`, `\/`, `\uXXXX` all
 * decode; anything unparseable falls back to the raw message. */
fun serverDetail(message: String): String {
    val m = Regex(""""detail"\s*:\s*"((?:[^"\\]|\\.)*)"""").find(message)
        ?: return message
    val raw = "\"" + m.groupValues[1] + "\""
    val detail = runCatching {
        JSONObject("{\"v\":$raw}").optString("v", "")
    }.getOrDefault("")
    return detail.trim().ifEmpty { message }
}

/**
 * Where a task-response contract mismatch goes (#185).
 *
 * A [StateFlow] rather than a plain `var` for two reasons, and the second is the one
 * that matters:
 *
 * 1. **Correctness.** `report()` runs on `Dispatchers.IO` and the screen reads on the
 *    main thread. Plain `var`s have no happens-before edge between those, so a UI read
 *    could miss the write — or see a half-published value. A `StateFlow` carries the
 *    value across the boundary correctly.
 * 2. **Reachability.** A sink nothing collects records the mismatch and shows nobody,
 *    which is the bug this whole PR exists to fix. A `StateFlow` has one obvious
 *    collector and the compiler-visible type says where the value goes.
 *
 * Only the NEWEST mismatch is held: a burst of editor autosaves would otherwise queue a
 * stack of stale warnings the user never dismisses, and only the newest describes the
 * current state. [consume] clears it once shown, so it is not re-displayed on
 * recomposition.
 */
internal object ContractWarnings {
    private val _mismatched = MutableStateFlow<ContractMismatch?>(null)

    /**
     * Newest mismatch, or null when there is nothing pending. Null rather than an empty
     * list so "nothing to show" and "a mismatch with no fields" cannot be confused —
     * [report] rejects the latter anyway, but the type should not have to carry that.
     */
    val mismatched: StateFlow<ContractMismatch?> = _mismatched.asStateFlow()

    /**
     * Advances on every report.
     *
     * An [AtomicInteger] rather than a plain counter: `report()` runs on
     * `Dispatchers.IO` and `generation++` is a read-modify-write, so two concurrent
     * autosaves could each read the same value and one increment would vanish.
     *
     * This is LOAD-BEARING, not observability: [consume] matches on it, so a lost
     * increment would let one warning clear another.
     */
    private val _generation = AtomicInteger(0)

    /** The current generation. Exposed for tests and diagnostics. */
    val generation: Int get() = _generation.get()

    fun report(fields: List<String>) {
        if (fields.isEmpty()) return
        _mismatched.value = ContractMismatch(_generation.incrementAndGet(), fields)
    }

    /**
     * Clears the warning **only if it is still the one that was shown**.
     *
     * [shown] is the generation passed to [message], not the field list, and that
     * distinction is the whole fix.
     *
     * The first version compared the LISTS:
     *
     *     collect[A] -> showSnackbar(A)      // suspends as long as the snackbar shows
     *     report(B)  -> value = [B]          // a second write lands meanwhile
     *     consume(A) -> clears iff value == A
     *
     * which holds only while A != B. Two reports of the SAME fields break it:
     * `report([X])`, `showSnackbar([X])`, `report([X])`, `consume([X])` clears the second
     * one — and same-fields is the COMMON case, not the exotic one, because both writes
     * are hitting the same broken server and get the same answer from it.
     *
     * Matching on a monotonic generation instead makes the two reports distinct values,
     * so clearing the first cannot touch the second however alike they look.
     */
    fun consume(shown: Int) {
        val current = _mismatched.value ?: return
        if (current.generation == shown) _mismatched.value = null
    }

    /**
     * The user-facing text. Names the fields: "something differs" sends them hunting.
     *
     * Deliberately does not say "reopen the task" — `reopen` is a real verb in this API
     * meaning the opposite (it un-completes a task), so telling a user to reopen a task
     * that is already saved reads as an instruction to undo it.
     *
     * Says "refreshing the list" rather than "open the task again", and the collector
     * makes that true by bumping `refreshTick`: the list is cached state the detail sheet
     * reads from, so an instruction to reopen would show the same stale row and read as
     * the app ignoring the user.
     */
    fun message(fields: List<String>): String =
        "Saved, but the server stored different values for " +
            fields.joinToString(", ") +
            ". Your app and the server may be on different versions — refreshing the list."
}

/**
 * One reported contract mismatch.
 *
 * Carries the generation so two reports of identical fields are distinguishable values.
 * Without it the pair collapses to the same list, and clearing one clears the other —
 * see [ContractWarnings.consume].
 */
internal data class ContractMismatch(
    val generation: Int,
    val fields: List<String>,
)
