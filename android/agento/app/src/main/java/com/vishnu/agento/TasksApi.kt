package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.core.content.edit
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
import org.json.JSONArray
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
    /** Optional verb phrase for voice reminders (#306); blank falls back to [name]. */
    val speakName: String = "",
    val description: String = "",
    val dueDate: String = "",
    val dueTime: String = "",
    val estimatedMinutes: Int = 0,
    // Recurrence: a structured interval (every N unit), a structured weekly
    // selection of days, or a custom condition (the user's own words in
    // repeatRule). 0/""/[]/false everywhere = one-shot.
    val repeatEvery: Int = 0,
    val repeatUnit: String = "",
    val repeatDays: List<String> = emptyList(),
    val repeatCustom: Boolean = false,
    val repeatRule: String = "",
    val parallelable: Boolean = false,
    // Optimistic-concurrency guard: bumped by every server-side mutation.
    // Sent back on update so a stale editor is rejected, not overwritten.
    val revision: Int = 0,
    val completedAt: String = "",
    val createdAt: String = "",
    /**
     * Derived server-side from `skippedAt` (#209): true when this occurrence
     * was skipped rather than done. A skipped task reads as "not done".
     */
    val skipped: Boolean = false,
    /**
     * The response contract the server spoke (#185). 0 means the server
     * predates versions entirely; anything else is asserted against
     * [TASK_CONTRACT_VERSION] on write, so a version disagreement warns
     * instead of defaulting every missing field to a genuine zero.
     */
    val contractVersion: Int = 0,
    /**
     * The next occurrence the server created by itself when a structured
     * cadence was completed ("" when there is none, or when the repeat is a
     * custom condition the caller has to schedule itself).
     */
    val nextDueDate: String = "",
)

/** A bounded list response: the rows, whether more exist, how many
 * rows arrived unparseable (empty id/name) and were skipped rather than
 * shown (#430), how many rows parsed only by resetting a mistyped
 * numeric field to its default (#437), and which resets those were
 * (#480 review): the first-3 sample rides the page for log parity —
 * a per-field sample of the counted rows, not a second total — while the
 * diagnostics record keeps every row's notes (see [lastCoercedNotes]), so
 * evicting single entries never drops detail. No screen displays
 * [coercedNotes] today: the hint's detail channel is the per-fetch record
 * About reads, and this field exists so the page's own sample stays
 * pinned by tests rather than implied by a global. A nonzero [dropped] means the server spoke rows this app
 * cannot read — the list surfaces the count instead of shrinking
 * silently. A nonzero [coerced] means some rows ARE shown but with a
 * reset value (e.g. `estimated_minutes: "abc"` read as 0) — the list
 * surfaces that count too, and the key, raw value and reset value ride
 * the log line so Settings → About diagnostics names them. */
data class TaskList(
    val tasks: List<ServerTask>,
    val truncated: Boolean = false,
    val dropped: Int = 0,
    val coerced: Int = 0,
    val coercedNotes: List<String> = emptyList(),
)

/**
 * The collection-wide change marker (#178): the revision the server is at, and
 * whether it is different from the one this device last saw.
 *
 * It is not a task, and it carries no task data on purpose. The question it
 * answers — "did anything change, including a write the agent made?" — used to
 * have no answer short of refetching the list and diffing it by eye, so the
 * only honest option was to refetch and hope.
 */
data class ChangeMarker(
    val revision: Long,
    val changed: Boolean,
)

/**
 * One op inside a batch (#345): complete, skip, update, or duplicate a
 * task by id. `reason` rides only on skip; `fields` only on update (the same
 * partial-update map the single-task update takes, e.g. due_date for a move
 * plus expected_revision for the concurrency guard). Construction details
 * live on the factory below; a private constructor forces every call site
 * through it.
 */
data class BulkOp private constructor(
    val op: String,
    val id: String,
    val reason: String? = null,
    val fields: JSONObject? = null,
) {
    companion object {
        // Normalized once, here: op/id trimmed, blank reason dropped to
        // null, fields deep-copied — so the set, the wire, the busy set, and
        // the error all name the same values, and no call site trims. After
        // construction, the op's fields are borrowed read-only (like
        // batch.go's shallow Fields copy): a future caller that needs to
        // change them must build a new op, never mutate the object —
        // mutating would move the wire and the per-item contract assert
        // together, making the drift self-consistent and invisible.
        operator fun invoke(
            op: String,
            id: String,
            reason: String? = null,
            fields: JSONObject? = null,
        ): BulkOp = BulkOp(
            op = op.trim(),
            id = id.trim(),
            reason = reason?.trim()?.ifEmpty { null },
            // Defensive copy: the factory borrows nothing. A caller
            // mutating the object after construction would otherwise change
            // the wire and the per-item contract assert together, making
            // the drift self-consistent and invisible.
            fields = fields?.let { JSONObject(it.toString()) },
        )
    }

    // Fail fast on misplaced extras: the server 422s a reason on non-skip
    // and fields on non-update, aborting the whole atomic batch — so a
    // caller bug here must read as a programming error, not a runtime 422.
    // Blank counts as absent (a blank reason arrives here as null), so only
    // a real value on the wrong verb throws.
    init {
        require(reason == null || op == "skip") { "reason rides only on skip" }
        require(fields == null || fields.isEmptyOrNullOnly() || op == "update") { "fields ride only on update" }
    }
}

/** What one committed batch item produced: the fresh task, plus the next
 * occurrence's due date when a structured cadence rolled over. */
data class BulkItemResult(
    val index: Int,
    val id: String,
    val op: String,
    val task: ServerTask,
    val nextDueDate: String = "",
)

/** Builds the `{"ops": [...]}` body for a bulk batch. Top-level (not a
 * member) so the shape is unit-testable without a Context. */
internal fun bulkOpsBody(ops: List<BulkOp>): JSONObject {
    val arr = JSONArray()
    for (op in ops) {
        // Values arrive normalized from BulkOp's factory; the body only
        // omits what counts as absent — a blank reason, a null-only or empty
        // fields object — mirroring the server's empty-as-absent rule.
        val o = JSONObject().put("op", op.op).put("id", op.id)
        if (!op.reason.isNullOrEmpty()) o.put("reason", op.reason)
        if (op.fields != null && !op.fields.isEmptyOrNullOnly()) o.put("fields", op.fields)
        arr.put(o)
    }
    return JSONObject().put("ops", arr)
}

/**
 * Which item of a bulk batch failed, recovered from the HTTP error text
 * (#345). The batch endpoint answers a failed op with the single-verb
 * status plus the op's index, id, and op, e.g.
 * `HTTP 409: {"detail": "conflict: ...", "index": 2, "id": "...", "op": "update"}`.
 */
internal data class BatchFailure(
    val detail: String,
    val index: Int,
    val id: String,
    val op: String,
)

/** True when the object carries no real values: empty, or every value an
 * explicit JSON null. Mirrors the server's empty-as-absent rule
 * (`ChangedFieldNames` treats null as absent). */
internal fun JSONObject.isEmptyOrNullOnly(): Boolean {
    val keys = keys()
    while (keys.hasNext()) {
        if (!isNull(keys.next())) return false
    }
    return true
}

/**
 * Field names the per-item contract assert understands. Anything else in a
 * bulk update's `fields` is a caller bug or a contract the app predates —
 * either way it must surface, not pass a weakened assert. `expected_revision`
 * is guard metadata, not a stored field, so it never counts.
 */
internal val BULK_UPDATE_KNOWN_FIELDS = setOf(
    "name", "speak_name", "description", "due_date", "due_time",
    "estimated_minutes", "repeat_every", "repeat_unit", "repeat_days",
    "repeat_custom", "repeat_rule", "parallelable", "expected_revision",
)

/** Wire keys whose change moves reminder speech or schedule (#470
 * review): the spoken text (name/voice phrase), the due instant, and the
 * estimate that derives the start. Matches [scheduleFingerprint] — repeat
 * and description ride no alert and no utterance, so bulk updates
 * touching only those keep live speech and notices, exactly like a
 * no-op single edit. `expected_revision` is guard metadata, never a
 * change. */
internal val SPEECH_SCHEDULE_UPDATE_KEYS = setOf(
    "name", "speak_name", "due_date", "due_time", "estimated_minutes",
)

/**
 * Whether a bulk update op's fields move speech/schedule state (#470
 * review): the spoken text (name/voice phrase), the due instant, and the
 * estimate that derives the start. Values compare, not keys — a
 * same-value rewrite (bulk-moving tasks already on that date) purges
 * nothing, exactly like a no-op single edit and the background
 * schedule-diff. Null fields, or no previous row to compare against,
 * mean "unknown": purge, fail-safe.
 */
internal fun updateMovesSpeech(fields: JSONObject?, prev: ServerTask? = null): Boolean {
    if (fields == null) return true
    val keys = mutableSetOf<String>()
    val it = fields.keys()
    while (it.hasNext()) keys.add(it.next())
    if (keys.none { it in SPEECH_SCHEDULE_UPDATE_KEYS }) return false
    if (prev == null) return true
    // Raw values, like the single edit: the wire trims server-side, so a
    // whitespace-only difference still counts as moved (safe direction —
    // the stored text may genuinely differ).
    fun str(key: String): String? =
        if (fields.has(key) && !fields.isNull(key)) fields.optString(key, "") else null
    fun num(key: String): Int? =
        if (fields.has(key) && !fields.isNull(key)) (fields.opt(key) as? Number)?.toInt() else null
    return str("name")?.let { it != prev.name } == true ||
        str("speak_name")?.let { it != prev.speakName } == true ||
        str("due_date")?.let { it != prev.dueDate } == true ||
        str("due_time")?.let { it != prev.dueTime } == true ||
        num("estimated_minutes")?.let { it != prev.estimatedMinutes } == true
}

/** Names the `fields` entries [bulkUpdateSent] cannot assert. Empty means
 * the op asserts everything it sent. */
internal fun bulkUnknownFields(fields: JSONObject): List<String> {
    val out = mutableListOf<String>()
    val keys = fields.keys()
    while (keys.hasNext()) {
        val key = keys.next()
        if (key !in BULK_UPDATE_KNOWN_FIELDS && !fields.isNull(key)) out.add(key)
    }
    return out.sorted()
}

/**
 * Builds the "what the app asked the server to store" record for one bulk
 * update op (#345), so a bulk move gets the same field-level assertion as a
 * single-task edit. Complete/skip/duplicate send no fields — there is
 * nothing to assert — so they stay version-only, exactly like the single
 * verbs. Top-level (not a member) so unit tests call it without a Context.
 */
internal fun bulkUpdateSent(fields: JSONObject): TaskContractSent {
    fun str(key: String): String? =
        if (fields.has(key) && !fields.isNull(key)) fields.optString(key, "") else null
    fun num(key: String): Int? =
        if (fields.has(key) && !fields.isNull(key)) (fields.opt(key) as? Number)?.toInt() else null
    fun flag(key: String): Boolean? =
        if (fields.has(key) && !fields.isNull(key)) fields.optBoolean(key) else null
    return TaskContractSent(
        name = str("name"),
        speakName = str("speak_name"),
        description = str("description"),
        dueDate = str("due_date"),
        dueTime = str("due_time"),
        estimatedMinutes = num("estimated_minutes"),
        repeatEvery = num("repeat_every"),
        repeatUnit = str("repeat_unit"),
        repeatDays = if (fields.has("repeat_days") && !fields.isNull("repeat_days")) {
            val raw = mutableListOf<String>()
            val arr = fields.optJSONArray("repeat_days")
            if (arr != null) {
                for (i in 0 until arr.length()) {
                    arr.optString(i, "").trim().takeIf { it.isNotEmpty() }?.let { raw.add(it) }
                }
            }
            canonicalRepeatDays(raw)
        } else {
            null
        },
        repeatCustom = flag("repeat_custom"),
        repeatRule = str("repeat_rule"),
        parallelable = flag("parallelable"),
    )
}

/** Recovers a [BatchFailure] from an HTTP error message, or null when the
 * message carries no batch envelope (a network failure, a proxy hint). */
internal fun parseBatchError(message: String): BatchFailure? {
    val start = message.indexOf('{')
    if (start < 0) return null
    val o = runCatching { JSONObject(message.substring(start)) }.getOrNull() ?: return null
    if (o.isNull("detail")) return null
    val detail = o.optString("detail", "").trim()
    if (detail.isEmpty()) return null
    return BatchFailure(
        detail = detail,
        index = o.optInt("index", -1),
        id = if (o.isNull("id")) "" else o.optString("id", ""),
        op = if (o.isNull("op")) "" else o.optString("op", ""),
    )
}

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

/** True when the task repeats at all: a structured cadence (interval or
 * selected days) or a custom condition. One-shot tasks must not open a
 * recreate draft. */
val ServerTask.hasRepeat: Boolean
    get() = repeatCustom || repeatEvery > 0 || repeatDays.isNotEmpty() || repeatRule.isNotEmpty()

/** Human repeat line, matching the server's rendering: "Every 3 days",
 * "Mon, Wed, Fri", the custom words verbatim, or "" for a one-shot task. */
fun ServerTask.repeatLabel(): String =
    repeatLabel(repeatEvery, repeatUnit, repeatDays, repeatCustom, repeatRule)

/** The same rendering for any source of the recurrence fields. */
fun repeatLabel(
    every: Int,
    unit: String,
    days: List<String>,
    custom: Boolean,
    text: String,
): String {
    if (days.isNotEmpty()) {
        // The server's Repeat.String, mirrored: the stored selection is already
        // canonical, so this never has to sort or de-duplicate.
        return days.joinToString(", ") { dayLabel(it) }
    }
    // every <= 0 rather than == 0: a stray or half-migrated doc must never
    // render "Every day" from a zero count.
    if (custom || every <= 0 || unit.isEmpty()) return text
    val singular = if (unit.endsWith("s")) unit.dropLast(1) else unit
    return if (every <= 1) "Every $singular" else "Every $every $unit"
}

/** True while the task is still open (never completed). */
fun ServerTask.isOpen(): Boolean = completedAt.isBlank()

/** Lenient string read: Android's `optString` coerces JSON null to
 * the string "null", so every nullable string field must go through
 * here (#130).
 *
 * Neutral name on purpose: the package already has a stricter top-level
 * `optStr` (MemoryApi) that blanks non-string values, which would
 * silently change number fields here if this shadowed it. */
internal fun optJsonStr(o: JSONObject, key: String): String =
    if (o.isNull(key)) "" else o.optString(key, "").trim()

/** Lenient weekday-selection read (#304): a missing or mistyped key is an
 * empty selection rather than a parse failure — the whole parse path is
 * lenient across shapes, and the round-trip compare is what reports a
 * server that stored something the app did not ask for. Values are
 * canonicalised on the way in so a display and a comparison both start from
 * the same spelling the server stores. */
internal fun optTaskDays(o: JSONObject, key: String): List<String> {
    val arr = o.optJSONArray(key) ?: return emptyList()
    val raw = mutableListOf<String>()
    for (i in 0 until arr.length()) {
        val s = arr.optString(i, "").trim()
        if (s.isNotEmpty()) raw.add(s)
    }
    return canonicalRepeatDays(raw)
}

/** Lenient int read that names its coercions (#437): int, long and
 * double arrive as [Number], and a numeric string (including a
 * double-encoded `"1.0"`) reads as its number. Deliberately stricter than
 * [parseContractVersion]: the version read stays silent-lenient on
 * fractions/`NaN`/overflow (unknown reads 0 = pre-version, reported via the
 * write-path version warnings), while a task numeric resets AND reports on
 * anything it cannot hold — a silent rewrite of a visible row is worse than
 * a silent rewrite of a handshake marker (#480 review). The gate is finite,
 * in-range AND
 * integral: non-numeric strings, booleans/objects/arrays, non-finite
 * doubles (`"NaN"`/`"Infinity"`), out-of-range ints/longs/doubles, and
 * fractions (`5.9`/`"5.9"`) all reset to [default] AND report the raw
 * value in [CoercedInt.raw] (null raw means clean, so only a reset
 * notifies). Absent or JSON-null reads [default] with a null raw:
 * a default itself is fine, only a reset is reported. Bounds stay at the
 * call site — but the call site notifies, because a clamped negative is
 * still a reset, not a default (see `parseTask`). */
internal data class CoercedInt(val value: Int, val raw: String?)

internal fun optTaskInt(o: JSONObject, key: String, default: Int = 0): CoercedInt {
    if (!o.has(key) || o.isNull(key)) return CoercedInt(default, null)
    val v = o.opt(key)
    // Range-checked through Double: a bare toInt() wraps out-of-range
    // Longs negative and saturates huge Doubles, both silently (#480
    // review). Anything unrepresentable resets AND reports. Integral-only
    // past that: a surviving fraction would truncate lossily, which is
    // the same silent rewrite unrepresentability is (#480 review).
    if (v is Number) {
        val d = v.toDouble()
        if (!d.isFinite() || d < Int.MIN_VALUE || d > Int.MAX_VALUE ||
            d.toInt().toDouble() != d
        ) {
            return CoercedInt(default, escapeTaskRawText(v.toString().take(40)))
        }
        return CoercedInt(v.toInt(), null)
    }
    if (v is String) {
        // Trimmed for parsing ("  7  " reads 7 clean), but reported
        // verbatim: the note must name what was stored, so a blank write
        // never reads as an empty one (#480 review). Control chars are
        // escaped so the log line stays one line.
        val t = v.trim()
        t.toIntOrNull()?.let { return CoercedInt(it, null) }
        // Finite, in-range AND integral: "NaN"/"Infinity" parse as Double
        // but are not numbers the task can hold, and "5.9" would truncate
        // (#480 review).
        t.toDoubleOrNull()?.takeIf { it.isFinite() }?.let { d ->
            if (d < Int.MIN_VALUE || d > Int.MAX_VALUE || d.toInt().toDouble() != d) {
                return CoercedInt(default, visibleTaskRaw(v))
            }
            return CoercedInt(d.toInt(), null)
        }
        return CoercedInt(default, visibleTaskRaw(v))
    }
    // Unquoted (unlike strings) but escaped the same: a JSONArray raw
    // carries quotes, and any raw can carry a newline (#480 review).
    return CoercedInt(default, escapeTaskRawText(v.toString().take(40)))
}

/**
 * What the note names for an unreadable value (#480): strings verbatim in
 * quotes — never the trimmed parse copy — with backslash, quote and
 * control chars escaped so one reset stays one log line. Capped like the
 * other raws.
 */
internal fun visibleTaskRaw(v: String): String =
    "\"" + escapeTaskRawText(v.take(40)) + "\""

/**
 * The escaping half of [visibleTaskRaw], shared by the non-string
 * fallbacks in [optTaskInt] (a `JSONArray` raw carries quotes too) and the
 * batch 404 matcher: backslash first, then quote and control chars, so a
 * hostile stored value cannot break the quoted note format or span lines.
 * List delimiters (`;`, `|`) are deliberately NOT escaped: the About line
 * is human-readable, not machine-parseable — a raw containing them reads
 * ambiguously, and mangling common values for a parser that does not exist
 * would trade a rare ambiguity for a certain one (#480 review).
 */
internal fun escapeTaskRawText(v: String): String =
    v.replace("\\", "\\\\").replace("\"", "\\\"")
        .replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t")

/**
 * Bounded diagnostics slot for one task row's single-read resets (#480):
 * the SHA-256 of the trimmed id, 12 hex chars (48 bits — collision odds
 * negligible at this cardinality, birthday bound ~2^24, but not zero;
 * never treat distinct hashes as distinct rows by guarantee). Raw ids
 * would let a pathological server grow the map without bound; a raw
 * prefix would alias two long ids into one slot (one row's clean read
 * clearing another's dirty entry). The hash is fixed-size for any input.
 * Record and clear sites must both go through here: deriving
 * one side from the URL-encoded id broke the eviction for exactly the
 * pathological ids the bound exists for (#480 review). The SHA-256
 * lookup itself never fails on JVM/Android; the take(24) fallback is
 * defence in depth and aliases by construction.
 */
internal fun singleCoercionState(id: String): String =
    runCatching {
        val md = java.security.MessageDigest.getInstance("SHA-256")
        val digest = md.digest(id.trim().toByteArray(Charsets.UTF_8))
        digest.take(6).joinToString("") { "%02x".format(it) }
    }.getOrDefault(id.trim().take(24))

/**
 * Which op ids a batch 404 names (#480): the server truncates echoed ids
 * to 40 bytes (`TruncateBytes`, store.go — mirrored by
 * [truncateUtf8Bytes]) and quotes them — JSON double quotes
 * in the echoed body, single quotes in the `unknown task` detail — so
 * a full-length match would miss exactly the long ids the bounded-key
 * work exists for, while a bare substring would let id "a" match a
 * message naming only "ab". Both quote forms bound the token on both
 * sides, each raw and JSON-escaped (backslash/quote/newline-class only —
 * Go's wider `\uXXXX`/HTML escapes are not mirrored, so an id needing
 * those still misses). Pure for tests.
 */
internal fun batchUnknownTaskIds(ids: List<String>, msg: String): List<String> {
    if ("HTTP 404:" !in msg || "unknown task" !in msg) return emptyList()
    // Group by echo first: two ids sharing a 40-byte prefix echo
    // identically, and clearing on a shared echo would evict a live sibling
    // with the gone row — the truncation dual of the substring aliasing
    // quoting fixed. Only a uniquely-echoed id auto-clears; colliding ones
    // keep their entries until a list page re-reads (or drops) the row
    // (#480 review).
    val echoes = ids.filter { it.isNotEmpty() }
        .groupBy { truncateUtf8Bytes(it.trim(), 40) }
    return echoes.values.filter { it.size == 1 }.flatten().filter { id ->
        val echo = truncateUtf8Bytes(id.trim(), 40)
        val escaped = escapeTaskRawText(echo)
        ("\"$echo\"" in msg || '$echo' in msg ||
            "\"$escaped\"" in msg || '$escaped' in msg)
    }
}

/** One task row: null when the row carries no usable id or name (#430
 * counts those drops in [TaskList.dropped] instead of shrinking the
 * list silently).
 *
 * Mistyped numerics reset to their defaults AND report through [coerced]
 * (#437): each entry names the key, the raw value and the reset value
 * (`estimated_minutes "abc"->0`), so the caller can log it once instead
 * of the reset passing silently. Null [coerced] means a single-row read
 * (`get`/`create`/`update`/`resolve`/`batch`): it logs the row's own line
 * directly (notifying once by construction) AND records under a bounded
 * hash of the row id ([singleCoercionState]) for diagnostics — otherwise
 * single-row resets would ride the log only, invisible after rotation
 * (#480 review). A clean single read clears its row's entry, and a list
 * page evicts the single entry of every row it contains (the page is the
 * newer vintage). A list caller passes its accumulator and logs one page
 * summary itself. */
internal fun parseTask(o: JSONObject, coerced: MutableList<String>? = null): ServerTask? {
    // Canonical `id` only (#433): the retired Mongo `_id` fallback is gone,
    // so a row that only carries `_id` is counted as unparseable instead of
    // being silently reinterpreted under a different wire contract.
    val id = optJsonStr(o, "id")
    if (id.isEmpty()) return null
    val name = optJsonStr(o, "name")
    if (name.isEmpty()) return null
    // One lenient read per numeric field (#437): a mistyped server value
    // used to become 0 minutes with no word. The note carries the row so
    // a page summary can tell two rows' resets apart.
    //
    // Single-row mode collects into a local list: the notes flush to the
    // log immediately (existing behaviour) and to the diagnostics record
    // at the end, so both channels hold for single rows too.
    val singleNotes = mutableListOf<String>()
    fun note(text: String) {
        if (coerced != null) {
            coerced.add(text)
        } else {
            singleNotes.add(text)
            runCatching {
                Log.w("TasksApi", "coerced $text")
            }
        }
    }
    fun num(key: String, default: Int = 0): CoercedInt {
        val r = optTaskInt(o, key, default)
        if (r.raw != null) {
            note("task ${id.take(24)}: $key ${r.raw}->${r.value}")
        }
        return r
    }
    // One lenient read with a floor per numeric field (#437, #480
    // review): a mistyped value resets to the default with a note, and a
    // well-typed negative is clamped to 0 with a note of its own — every
    // numeric reset notifies, not just the estimate's. The revision floor
    // matches its "0 = pre-version/backfill" rule (#184): a negative
    // revision is corruption, never a version.
    fun clampedNum(key: String, default: Int = 0): Int {
        val r = num(key, default)
        val v = r.value.coerceAtLeast(0)
        if (v != r.value) {
            note("task ${id.take(24)}: $key ${r.value}->$v")
        }
        return v
    }
    // estimated_minutes may encode as int, long, or double.
    val mins = clampedNum("estimated_minutes")
    // Pure: no prefs here. list() parses up to 200 rows per refresh, so a
    // read-modify-write per row is 200 I/Os for one fact — and a
    // read-then-write that is not atomic can regress (sees v3, then a late
    // v2 write wins). The callers record the batch max once, under one
    // lock, instead (#185 review).
    val version = parseContractVersion(o.opt("contract_version"))
    return ServerTask(
        id = id,
        name = name,
        speakName = optJsonStr(o, "speak_name"),
        description = optJsonStr(o, "description"),
        dueDate = optJsonStr(o, "due_date"),
        dueTime = optJsonStr(o, "due_time"),
        estimatedMinutes = mins,
        repeatEvery = clampedNum("repeat_every"),
        repeatUnit = optJsonStr(o, "repeat_unit"),
        repeatDays = optTaskDays(o, "repeat_days"),
        repeatCustom = o.optBoolean("repeat_custom", false),
        repeatRule = optJsonStr(o, "repeat_rule"),
        parallelable = o.optBoolean("parallelable", false),
        // Missing on pre-revision servers: 0 matches the backfill, so
        // an old server and a new app still agree (#184).
        revision = clampedNum("revision"),
        completedAt = optJsonStr(o, "completedAt"),
        createdAt = optJsonStr(o, "createdAt"),
        contractVersion = version,
        skipped = o.optBoolean("skipped", false),
    ).also {
        // Single-row reads join both channels too: record under a bounded
        // hash of the row id (or clear it on a clean read, so a fixed row
        // stops advertising). The hash — not a raw prefix — is the key:
        // hex ObjectIds hash 1:1, and a pathological server id can neither
        // grow the map unboundedly nor alias another row's slot (#480
        // review). The human-readable `task <id.take(24)>:` prefix stays in
        // the note text, where a collision is display-only. List callers
        // pass an accumulator and skip the record — the page records once
        // for the whole batch, and owns evicting the single entries of the
        // rows it contains (see parseTaskListPage) — so a sourceless parse
        // never mutates diagnostics it promised not to touch (#480 review).
        if (coerced == null) {
            TasksApi.noteCoercedNotes("single", singleCoercionState(id), singleNotes.take(3))
        }
    }
}

/** One message for a server that does not speak the canonical task list
 * (#433): names the missing shape and the fix, never a clean empty page. */
internal const val TASKS_SHAPE_ERROR =
    "Unexpected response shape — the server sent no tasks list. Update the server."

/** Parses one task-list page (#430): the canonical `tasks` envelope, the
 * rows, how many rows arrived unparseable, and how many rows parsed only
 * by resetting a mistyped numeric field to its default (#437).
 *
 * [state] and [source] name the fetch for the diagnostics record
 * ([noteCoercedNotes]): the widget pulls three states, the Task Manager
 * one, reminders another — keying by fetch (source + state) stops one
 * screen's fetch clobbering or clearing another's detail (#480 review).
 * Blank in tests and direct calls (no record written without a source).
 *
 * The `data`/`items` fallbacks are gone (#433): a body without a `tasks`
 * array is a loud parse failure with an actionable message, never a clean
 * empty that reads as "no tasks". */
internal fun parseTaskListPage(body: String, state: String = "", source: String = ""): TaskList {
    val out = mutableListOf<ServerTask>()
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(TASKS_SHAPE_ERROR)
    val arr = root.optJSONArray("tasks")
        ?: throw RuntimeException(TASKS_SHAPE_ERROR)
    var dropped = 0
    val coercedNotes = mutableListOf<String>()
    var coerced = 0
    for (i in 0 until arr.length()) {
        val before = coercedNotes.size
        val obj = arr.optJSONObject(i)
        val task = obj?.let { parseTask(it, coercedNotes) }
        if (task != null) {
            out.add(task)
            if (coercedNotes.size > before) coerced++
        } else {
            dropped++
        }
        // The page is the newer vintage for every row it contains —
        // parsed or dropped: a row turning unparseable (blanked name) must
        // not leave its old parseable vintage behind forever, since no
        // future parseable page will ever evict it (#480 review). But only
        // for a real fetch: a sourceless parse writes no record at all
        // (see the KDoc), and must not delete either.
        if (source.isNotEmpty()) {
            val rowId = task?.id ?: obj?.let { optJsonStr(it, "id") }.orEmpty()
            if (rowId.isNotEmpty()) {
                TasksApi.noteCoercedNotes("single", singleCoercionState(rowId), emptyList())
            }
        }
    }
    if (dropped > 0) {
        // Diagnostics for the readers with no screen: the widget and
        // reminder paths take `.tasks` and never see the count, but
        // their log lines surface in Settings → About diagnostics.
        // runCatching: Log is stubbed (throws) on the JVM unit-test
        // classpath, same guard as ApprovalsApi.
        runCatching {
            Log.w("TasksApi", "dropped $dropped unparseable task row(s) in list response")
        }
    }
    if (coerced > 0) {
        // One line per page, not per row: the rows stay shown (with reset
        // values), and this names what was reset so a version-skewed
        // writer is visible. Capped detail — the count is the signal. The
        // same first-3 slice rides the page ([TaskList.coercedNotes]) — the log
        // and the page sample agree, while the diagnostics record keeps all rows'
        // notes (see below) so nothing evicted is lost (#480 review).
        runCatching {
            Log.w("TasksApi", "coerced $coerced task row(s) to defaults in list response: " +
                coercedNotes.take(3).joinToString("; ").take(400))
        }
    }
    // The diagnostics screen reads this per fetch, so one screen's
    // hint points at its own rows — and a clean page clears only its own
    // fetch's entry, never another screen's (#480 review). The full
    // note list lands here (page-bounded: at most one page of rows, replaced
    // wholesale per fetch), while the log line above and [TaskList.coercedNotes]
    // keep the first-3 sample — so evicting the single entries never deletes
    // detail the fetch does not keep: every evicted row's notes survive here
    // (#480 review).
    if (source.isNotEmpty()) {
        TasksApi.noteCoercedNotes(source, state, coercedNotes.toList())
    }
    return TaskList(out, root.optBoolean("truncated", false), dropped, coerced, coercedNotes.take(3))
}

/**
 * Client for the user's tasks (same bearer auth as chat/sync, served
 * through the proxy's /api/ route — no profile prefix; the collection is
 * global, not per-assistant). Full CRUD against the same store and
 * validation the agent uses over MCP. Row fields are lenient (missing reads
 * blank, rows without an id or name are skipped and counted
 * ([TaskList.dropped])), but the list envelope is the canonical `tasks`
 * array only — any other shape is a parse failure, never a clean empty
 * (#430, #433).
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
         * On the companion, not the instance: `TasksApi` takes a `Context`, so an instance
         * member is not callable from a plain JVM unit test — which is exactly where this
         * has to be callable from to be worth anything.
         *
         * Null in means null out: `update` passes only what it sent, and those stay null so
         * `contractMismatches` skips them. Strings are trimmed HERE, once, so the app sends
         * exactly what it asserts.
         */
        internal fun buildTaskContractSent(
            name: String? = null,
            speakName: String? = null,
            description: String? = null,
            dueDate: String? = null,
            dueTime: String? = null,
            estimatedMinutes: Int? = null,
            repeatEvery: Int? = null,
            repeatUnit: String? = null,
            repeatDays: List<String>? = null,
            repeatCustom: Boolean? = null,
            repeatRule: String? = null,
            parallelable: Boolean? = null,
        ): TaskContractSent = TaskContractSent(
            name = name?.trim(),
            // Trimmed AND capped, exactly like the server stores it — so a
            // pasted paragraph asserts as what was stored, not as what was typed.
            speakName = speakName?.trim()?.let { truncateUtf8Bytes(it, SPEAK_NAME_MAX_BYTES) },
            description = description?.trim(),
            dueDate = dueDate?.trim(),
            dueTime = dueTime?.trim(),
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit?.trim(),
            // Canonicalised here, next to the trims, for the same reason: the
            // body is written from `sent`, so what is asserted has to be what is
            // sent. Null in means null out (an update that did not touch the
            // selection asserts nothing about it).
            repeatDays = repeatDays?.let { canonicalRepeatDays(it) },
            repeatCustom = repeatCustom,
            repeatRule = repeatRule?.trim(),
            parallelable = parallelable,
        )

        // One shared client: each OkHttpClient owns a connection pool and
        // dispatcher threads, so per-call instances would leak both.
        private val sharedHttp: OkHttpClient by lazy {
            OkHttpClient.Builder()
                .connectTimeout(10, TimeUnit.SECONDS)
                .readTimeout(30, TimeUnit.SECONDS)
                .writeTimeout(30, TimeUnit.SECONDS)
                .build()
        }

        /**
         * Guards the read-modify-write in [TasksApi.noteContractVersion].
         * On the companion, not the instance: there is one TasksApi per
         * screen, so an instance lock would not order two concurrent
         * refreshes against each other — which is the interleaving that
         * could land the older max last.
         */
        private val versionLock = Any()

        /**
         * The latest numeric-reset notes per fetch (#437, #480): each
         * names the row, the key, the raw value and what it became — every
         * row of the page, not the first-3 sample the log and [TaskList] carry
         * (page-bounded and replaced wholesale per fetch, so it cannot grow
         * without bound). Keyed by fetch (`source:state` — the Task Manager
         * pulls one state, the widget push three, each widget factory one
         * in its own per-placement namespace, reminders one) because any
         * single slot lets one fetch clobber or clear another's detail
         * — including same-state
         * fetches at different limits. Read by [TaskWidget.diagnostics]
         * so each screen's hint points at detail that is actually on that
         * screen. A clean page clears only its own entry; sibling states
         * under one source survive parallel writes, and each surface that
         * switches states (manager, widget factory) prunes its stale
         * siblings explicitly ([clearCoercedNotesExcept]); when a source's
         * last writer goes away its entries go with it
         * ([clearCoercedNotesSource]).
         */
        @Volatile
        internal var lastCoercedNotes: Map<String, List<String>> = emptyMap()
            private set

        /** Records one fetch's notes (see [lastCoercedNotes]). */
        internal fun noteCoercedNotes(source: String, state: String, notes: List<String>) {
            // No pruning here: parallel writers under one source (the
            // widget's three state fetches) must all survive — pruning
            // same-source siblings on every write keeps only the last
            // completer (#480 review). Sibling pruning lives with the one
            // screen that switches states ([clearCoercedNotesExcept]).
            synchronized(versionLock) {
                lastCoercedNotes = if (notes.isEmpty()) {
                    lastCoercedNotes - "$source:$state"
                } else {
                    lastCoercedNotes + ("$source:$state" to notes)
                }
            }
        }

        /**
         * Drops one source's entries for every state but [keepState].
         * The Task Manager (one state per filter) and each widget factory
         * (one state per bound placement, in its own per-placement
         * namespace) prune to the settled state: without this
         * the old filter's/view's notes would linger next to the new page (#480
         * review). Called when a surface settles on rows: the manager on
         * each successful load, the factory on each bind that settled on
         * rows (cache hit or landed fetch — never a failed fetch showing
         * nothing, never a preview bind), mirroring the manager's
         * success-only call. Writers with a fixed state set
         * (reminders, the widget push path) never need it.
         */
        internal fun clearCoercedNotesExcept(source: String, keepState: String) {
            synchronized(versionLock) {
                lastCoercedNotes = lastCoercedNotes.filterKeys {
                    it == "$source:$keepState" || !it.startsWith("$source:")
                }
            }
        }

        /**
         * Drops every entry for one source (#480 review): the task-notes
         * equivalent of the widget-pref prune. Called when the source's last
         * writer goes away — no placements left means no fetch will ever
         * refresh (or clear) those entries again, so they would read as
         * current state until process restart while being history.
         */
        internal fun clearCoercedNotesSource(source: String) {
            synchronized(versionLock) {
                lastCoercedNotes = lastCoercedNotes.filterKeys { !it.startsWith("$source:") }
            }
        }
    }

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http: OkHttpClient get() = sharedHttp

    /** The one canonical endpoint (#433): [ServerEndpoint] owns the key and
     * the shape, so this client cannot drift onto a retired one. */
    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

    /**
     * Records a task contract version: the running max of what this install
     * has spoken (#185).
     *
     * A stale install is otherwise diagnosable only by guesswork: the symptom
     * of a contract mismatch is "the app is wrong", not "the versions
     * disagree". The stored max says which contracts this install has actually
     * spoken, without a new endpoint to ask — and it is surfaced in
     * Settings → About diagnostics, because a value nothing reads is not
     * diagnosability, it is a write. Zeros are not recorded — absent is
     * "unknown", not a version.
     *
     * Read-modify-write under one lock: this runs on Dispatchers.IO and two
     * concurrent refreshes could otherwise interleave and land the older max
     * last.
     */
    private fun noteContractVersion(version: Int) {
        if (version <= 0) return
        synchronized(versionLock) {
            if (prefs.getInt(CONTRACT_VERSION_SEEN_KEY, 0) < version) {
                prefs.edit { putInt(CONTRACT_VERSION_SEEN_KEY, version) }
            }
        }
    }

    /** Records the highest contract a response batch spoke. */
    private fun noteContractVersion(tasks: List<ServerTask>) =
        noteContractVersion(maxContractVersion(tasks))

    /**
     * Lists tasks by state (`open`, `done`, `all`); unknown states fail fast.
     *
     * One bounded page: [limit] <= 0 takes the server default (200, max
     * 500), and [TaskList.truncated] says whether more rows exist, so the
     * list never silently ends mid-collection (#168).
     *
     * [source] names the caller for the diagnostics record
     * ([noteCoercedNotes]): each screen's fetch keeps its own entry.
     */
    suspend fun list(state: String = "open", limit: Int = 0, source: String = ""): Result<TaskList> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
            }
            val s = state.trim().lowercase(Locale.ROOT)
            if (s != "open" && s != "done" && s != "all") {
                return@withContext Result.failure(IllegalArgumentException("Unknown state: $state"))
            }
            val query = if (limit > 0) "/api/tasks?state=$s&limit=$limit" else "/api/tasks?state=$s"
            call("GET", query).map { body ->
                parseTaskListPage(body, s, source)
            }.also { r -> r.getOrNull()?.let { noteContractVersion(it.tasks) } }
        }

    /**
     * The change marker as the server sees it right now (#178).
     *
     * [since] is the revision this device last observed — 0 if it has never
     * looked. The answer's `changed` is the honest one for a server that
     * predates this endpoint: a call that cannot be answered comes back as a
     * failure, and every caller treats that as "assume changed" rather than
     * as "nothing happened", because a fetch the user did not need is
     * cheaper than a screen nobody updated.
     */
    suspend fun changes(since: Long): Result<ChangeMarker> =
        withContext(Dispatchers.IO) {
            call("GET", "/api/tasks/changes?since=$since").map { body ->
                val root = JSONObject(body)
                // optBoolean defaults to true for the same reason as above:
                // a marker that cannot say "no" must not imply one.
                ChangeMarker(
                    revision = root.optLong("revision", 0L),
                    changed = root.optBoolean("changed", true),
                )
            }
        }

    /**
     * One task by id, or null when it is gone (#168).
     *
     * A 404 whose body names the unknown task means done/deleted after
     * arming; anything else (transport, wrong server) stays a failure, so
     * the caller can tell "finished" from "unknowable". The match lives
     * here, next to the 404 handling, rather than in every caller.
     */
    suspend fun get(id: String): Result<ServerTask?> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            // mapCatching, not map: a malformed doc must come back as a
            // failure (generic alert + rearm), never as a throw escaping
            // the receiver's coroutine.
            call("GET", "/api/tasks/$clean").mapCatching { body ->
                parseOne(body)
            }.fold(
                onSuccess = { task ->
                    task?.let { noteContractVersion(listOf(it)) }
                    Result.success(task)
                },
                onFailure = { e ->
                    // Code and body: a gateway 404 (wrong server) must never
                    // read as a finished task.
                    val msg = e.message ?: ""
                    if ("HTTP 404:" in msg && "unknown task" in msg) {
                        // Gone server-side: drop any diagnostics entry for
                        // it, or About names a reset for a task that no
                        // longer exists (#480 review).
                        clearSingleIfGone(id, msg)
                        Result.success(null)
                    } else {
                        Result.failure(e)
                    }
                },
            )
        }

    /** Creates a task. Everything except the repeat is required — the
     * server rejects missing keys (all-zero repeat = one-shot). The repeat is
     * one of three shapes — an interval cadence, a selected-days cadence, or a
     * custom condition — never a combination. */
    suspend fun create(
        name: String,
        description: String,
        dueDate: String,
        dueTime: String,
        estimatedMinutes: Int,
        repeatEvery: Int = 0,
        repeatUnit: String = "",
        repeatDays: List<String> = emptyList(),
        repeatCustom: Boolean = false,
        repeatRule: String = "",
        parallelable: Boolean = false,
        // Optional voice phrase for TTS reminders (#306); blank = fallback to name.
        // Fully IN the contract assert (see TaskContractSent): old servers just
        // ignore the unknown key on the wire (taskbody_test pins that tolerance),
        // and answer without it — which the version check then reports.
        speakName: String = "",
    ): Result<ServerTask> = withContext(Dispatchers.IO) {
        if (name.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Name is required"))
        }
        if (description.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Description is required"))
        }
        if (dueDate.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Due date is required"))
        }
        if (dueTime.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Due time is required"))
        }
        if (estimatedMinutes < 0) {
            return@withContext Result.failure(IllegalArgumentException("Estimated minutes must be 0 or above"))
        }
        // #185: trimmed once, and the SAME values are asserted and sent. Trimming only
        // in the comparison would leave the two able to disagree on padding, which the
        // trim-insensitive compare then hides — so the app would depend on the server
        // cleaning up before they agree.
        val sent = buildTaskContractSent(
            name = name,
            speakName = speakName,
            description = description,
            dueDate = dueDate,
            dueTime = dueTime,
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit,
            repeatDays = repeatDays,
            repeatCustom = repeatCustom,
            repeatRule = repeatRule,
            parallelable = parallelable,
        )
        // The body reads its strings back OUT of `sent`: two trims are two rules, and the
        // one that drifts is the one the trim-insensitive compare hides.
        //
        // Unreachable by the types — `create` takes non-null parameters, so the shared
        // builder cannot return null for any of them. Kept anyway, and kept IN-CHANNEL:
        // `JSONObject.put(String, Object?)` silently DROPS a null key, so if these
        // parameters ever become nullable the create would quietly lose a field instead
        // of failing. A `require` here would throw out of this suspend, and callers use
        // `.fold(onFailure = ::fail)` with no try/catch — so the failure would escape the
        // coroutine and show no toast at all.
        if (sent.name == null || sent.speakName == null || sent.description == null || sent.dueDate == null ||
            sent.dueTime == null || sent.estimatedMinutes == null || sent.parallelable == null ||
            sent.repeatEvery == null || sent.repeatUnit == null || sent.repeatDays == null ||
            sent.repeatCustom == null || sent.repeatRule == null
        ) {
            return@withContext Result.failure(
                IllegalStateException("create: builder dropped a required field"),
            )
        }
        // The body is written from TASK_CREATE_KEYS, not from a second literal
        // list of puts: one key list means the list cannot drift from what is
        // actually sent, and the CI drift step asserts this set equals the
        // server's taskCreateFields (#185 review).
        //
        // A missing key fails IN-CHANNEL, like the null-guard above: the
        // callers fold Result.failure into a toast, while a throw (require,
        // getValue) would escape the coroutine and show nothing at all.
        val values: Map<String, Any?> = mapOf(
            "name" to sent.name,
            "speak_name" to sent.speakName,
            "description" to sent.description,
            "due_date" to sent.dueDate,
            "due_time" to sent.dueTime,
            "estimated_minutes" to sent.estimatedMinutes,
            "parallelable" to sent.parallelable,
            "repeat_every" to sent.repeatEvery,
            "repeat_unit" to sent.repeatUnit,
            // An explicit JSONArray rather than the List itself: JSONObject.put
            // accepts any Object, so a raw Collection would rely on the writer
            // to notice it is one. Building the array here keeps the wire shape
            // visible at the one place that decides it.
            "repeat_days" to JSONArray(sent.repeatDays),
            "repeat_custom" to sent.repeatCustom,
            "repeat_rule" to sent.repeatRule,
        )
        val body = JSONObject()
        for (k in TASK_CREATE_KEYS) {
            // A null value would be silently DROPPED by put, sending a short
            // body — so null fails here, the same as a missing key.
            val v = values[k] ?: return@withContext Result.failure(
                IllegalStateException("create: no value for key $k"),
            )
            body.put(k, v)
        }
        call("POST", "/api/tasks", body).map { parseOne(it) }
            .also { r ->
                r.getOrNull()?.let {
                    noteContractVersion(listOf(it))
                    checkContract(it, sent)
                }
            }
    }

    /**
     * Partial edit: only non-null keys are sent.
     *
     * The four repeat keys are CONVENTIONALLY sent together or not at all, so the server
     * validates the recurrence as a whole rather than merging half of it. That is a
     * convention, not an invariant of this signature: each key is an independent nullable
     * parameter and each is written to the body independently, so a caller CAN send
     * `repeat_every` alone and the server will merge it into the stored recurrence.
     *
     * The comment used to say "sent together or not at all" as though the method
     * guaranteed it. It never did, and a comment asserting a guarantee the code does not
     * make is worse than no comment — it is the stale-prose shape that ships wrong advice,
     * the same defect as the `exhausted` sentence in #180 that told users to re-enter an
     * intact repeat.
     *
     * Enforcing it would mean rejecting a half-repeat update outright, which changes the
     * API contract and belongs with the other deferred parts of #185, not in a KDoc fix.
     * (#304 adds `repeatDays` as a fifth key with the same convention.)
     */
    suspend fun update(
        id: String,
        name: String? = null,
        speakName: String? = null,
        description: String? = null,
        dueDate: String? = null,
        dueTime: String? = null,
        estimatedMinutes: Int? = null,
        repeatEvery: Int? = null,
        repeatUnit: String? = null,
        repeatDays: List<String>? = null,
        repeatCustom: Boolean? = null,
        repeatRule: String? = null,
        parallelable: Boolean? = null,
        // The revision the editor read: when someone else wrote first the
        // server answers 409 instead of overwriting (#184).
        expectedRevision: Int? = null,
    ): Result<ServerTask> = withContext(Dispatchers.IO) {
        val clean = encodeId(id)
        if (clean.isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Missing task id"))
        }
        // ONE record, built once, and the body is written from it — exactly what
        // `create` does. This used to trim here AND inside `buildTaskContractSent`,
        // which is the same value today and two rules tomorrow: change the builder's
        // trimming and the body silently diverges, and the trim-insensitive compare
        // hides it, so the drift is invisible until a server stops trimming for us.
        //
        // A null field means "not sent", so it is omitted from the body entirely rather
        // than sent as a JSON null — which the server would read as an explicit clear.
        val sent = buildTaskContractSent(
            name = name,
            speakName = speakName,
            description = description,
            dueDate = dueDate,
            dueTime = dueTime,
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit,
            repeatDays = repeatDays,
            repeatCustom = repeatCustom,
            repeatRule = repeatRule,
            parallelable = parallelable,
        )
        val body = JSONObject()
        sent.name?.let { body.put("name", it) }
        sent.speakName?.let { body.put("speak_name", it) }
        sent.description?.let { body.put("description", it) }
        sent.dueDate?.let { body.put("due_date", it) }
        sent.dueTime?.let { body.put("due_time", it) }
        sent.estimatedMinutes?.let { body.put("estimated_minutes", it) }
        sent.repeatEvery?.let { body.put("repeat_every", it) }
        sent.repeatUnit?.let { body.put("repeat_unit", it) }
        // An empty list is a REAL value here: it is how the app says "this task
        // no longer repeats on selected days". `?.let` keeps a null (not sent)
        // distinct from it, exactly like the strings above.
        sent.repeatDays?.let { body.put("repeat_days", JSONArray(it)) }
        sent.repeatCustom?.let { body.put("repeat_custom", it) }
        sent.repeatRule?.let { body.put("repeat_rule", it) }
        sent.parallelable?.let { body.put("parallelable", it) }
        if (expectedRevision != null) body.put("expected_revision", expectedRevision)
        call("PATCH", "/api/tasks/$clean", body).map { parseOne(it) }
            .also { r -> r.exceptionOrNull()?.let { clearSingleIfGone(id, it.message ?: "") } }
            .also { r ->
                r.getOrNull()?.let { task ->
                    // Only the fields this call actually sent are asserted; the rest stay
                    // null, which contractMismatches skips. The previous version built
                    // the expected value FROM the response and copied the sent fields
                    // over it — correct, because the unsent fields then matched
                    // themselves, but fragile: a field added to one side and not the
                    // other becomes silently "asserted" as whatever the server said.
                    // Nulls put the decision at this call site, where it is visible —
                    // and `sent` is the SAME record the body was written from, so what
                    // is asserted is what was sent rather than a second construction of it.
                    // The app knows which task it asked to change, so a response
                    // carrying a DIFFERENT id is detectable — a server answering with
                    // the wrong record is otherwise invisible when every field matches.
                    noteContractVersion(listOf(task))
                    checkContract(task, sent, expectedId = id)
                }
            }
    }

    /** Marks a task done (server starts the 3-day retention clock). The
     * response carries the rolled-over occurrence in `next` when a
     * structured cadence produced one. */
    suspend fun complete(id: String): Result<ServerTask> = resolve(id, "complete")

    /**
     * Skips the current occurrence without claiming the work was done
     * (#287). Same envelope as [complete] (task + optional `next` +
     * `rollover`); a structured repeat rolls over server-side. The task
     * reads back with `skipped = true` — "not done", never "done".
     *
     * No reason is sent: the notification action carries no text field, so
     * `skipReason` stays empty on this path (the agent's `skip_task` is
     * what writes one).
     */
    suspend fun skip(id: String): Result<ServerTask> = resolve(id, "skip")

    /**
     * Shared complete/skip body: both verbs answer the same envelope
     * (task + optional `next` + `rollover`). One copy, so the next
     * contract fix cannot land on one verb and miss the other.
     */
    private suspend fun resolve(id: String, verb: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("POST", "/api/tasks/$clean/$verb", JSONObject()).map { body ->
                val root = JSONObject(body)
                val task = parseTask(root.optJSONObject("task") ?: root)
                    ?: throw RuntimeException(TASKS_SHAPE_ERROR)
                val next = root.optJSONObject("next")
                // Each half versioned on its own (#185 review): maxOf would
                // collapse task=1 + pre-version next=0 to 1 and hide the
                // half-disagreement — in both directions — and next is the
                // half that already broke once (#180). Handled here rather
                // than in an also below, which would only ever see the task
                // half. Read leniently, like the top-level version: this must
                // never fail a completion, only version it.
                val nextVersion =
                    next?.let { parseContractVersion(it.opt("contract_version")) }
                if (completeVersionMismatch(task.contractVersion, nextVersion)) {
                    ContractWarnings.report(listOf("contract_version"), task.name)
                }
                noteContractVersion(maxOf(task.contractVersion, nextVersion ?: 0))
                if (next == null) {
                    task
                } else {
                    // optJsonStr, not optString: a JSON null would arrive as the
                    // literal string "null" and reach the toast.
                    task.copy(nextDueDate = optJsonStr(next, "due_date"))
                }
            }.also { r -> r.exceptionOrNull()?.let { clearSingleIfGone(id, it.message ?: "") } }
        }

    /**
     * One atomic bulk run (#345): complete, skip, update, or duplicate a set
     * of tasks in one server transaction. The whole batch commits or nothing
     * does; a hard failure names the failing op (index/id/op) with the
     * single-verb status, so the screen can report WHICH item failed and keep
     * the selection for retry.
     */
    suspend fun batch(ops: List<BulkOp>): Result<List<BulkItemResult>> =
        withContext(Dispatchers.IO) {
            if (ops.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Nothing to send"))
            }
            call("POST", "/api/tasks/batch", bulkOpsBody(ops)).map { text ->
                val arr = JSONObject(text).optJSONArray("results")
                    ?: throw RuntimeException(TASKS_SHAPE_ERROR)
                val out = mutableListOf<BulkItemResult>()
                for (i in 0 until arr.length()) {
                    val item = arr.optJSONObject(i)
                        ?: throw RuntimeException(TASKS_SHAPE_ERROR)
                    val root = item.optJSONObject("task")
                        ?: throw RuntimeException(TASKS_SHAPE_ERROR)
                    var task = parseTask(root)
                        ?: throw RuntimeException(TASKS_SHAPE_ERROR)
                    // No lenient fallback to task.id: the server guarantees
                    // ID (source id, or the new copy's id for duplicate), so
                    // a missing one is a contract violation, not a gap to
                    // paper over.
                    val id = optJsonStr(item, "id")
                    if (id.isEmpty()) throw RuntimeException(TASKS_SHAPE_ERROR)
                    // Version handling mirrors resolve(): each half versioned
                    // on its own, warn-not-throw on disagreement.
                    val next = item.optJSONObject("next")
                    val nextVersion =
                        next?.let { parseContractVersion(it.opt("contract_version")) }
                    if (completeVersionMismatch(task.contractVersion, nextVersion)) {
                        ContractWarnings.report(listOf("contract_version"), task.name)
                    }
                    noteContractVersion(maxOf(task.contractVersion, nextVersion ?: 0))
                    if (next != null) {
                        task = task.copy(nextDueDate = optJsonStr(next, "due_date"))
                    }
                    // Field-level assertion for update ops, mirroring the
                    // single-task edit: the request op carries the fields,
                    // matched by the server's index — never by array
                    // position, so a future server that omits or reorders
                    // the index runs no check rather than checking the move
                    // against the wrong op's fields.
                    // Complete/skip/duplicate send no fields, so they stay
                    // version-only — exactly like the single verbs.
                    if (item.has("index")) {
                        ops.getOrNull(item.optInt("index", -1))?.let { req ->
                            if (req.op == "update" && req.fields != null) {
                                val unknown = bulkUnknownFields(req.fields)
                                if (unknown.isNotEmpty()) {
                                    ContractWarnings.report(unknown.map { "fields:$it" }, task.name)
                                }
                                checkContract(task, bulkUpdateSent(req.fields), expectedId = id)
                            }
                        }
                    }
                    out.add(
                        BulkItemResult(
                            index = item.optInt("index", i),
                            id = id,
                            op = optJsonStr(item, "op"),
                            task = task,
                            nextDueDate = task.nextDueDate,
                        ),
                    )
                }
                out.toList()
            }.also { r ->
                // A whole-call 404 names no row in its URL: drop only the
                // entries the error itself names, so live rows keep theirs
                // (#480 review). Success needs no clear — each item parse
                // just re-recorded its row's current entry.
                for (id in batchUnknownTaskIds(ops.map { it.id }, r.exceptionOrNull()?.message ?: "")) {
                    noteCoercedNotes("single", singleCoercionState(id), emptyList())
                }
            }
        }

    /** Reopens a done task. */
    suspend fun reopen(id: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("POST", "/api/tasks/$clean/reopen", JSONObject()).map { parseOne(it) }
                .also { r -> r.exceptionOrNull()?.let { clearSingleIfGone(id, it.message ?: "") } }
                .also { r ->
                    r.getOrNull()?.let {
                        noteContractVersion(listOf(it))
                        checkVersion(it)
                    }
                }
        }

    /** Permanently deletes a task. */
    suspend fun delete(id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("DELETE", "/api/tasks/$clean").map { }.also {
                // Gone, or already gone: a 404 naming the row means it is
                // gone regardless of verb — either way no fetch will ever
                // refresh this entry again (#480 review).
                if (it.isSuccess) {
                    noteCoercedNotes("single", singleCoercionState(id), emptyList())
                } else {
                    clearSingleIfGone(id, it.exceptionOrNull()?.message ?: "")
                }
            }
        }

    /**
     * Compares a write's response against what was asked for, and surfaces any field
     * the server stored differently.
     *
     * A toast, not an exception: the write genuinely succeeded, so failing it would
     * tell the user their task was not saved when it was. That is the same class of lie
     * as #214 — reporting an outcome that did not happen — and it is the reason this
     * warns instead of throws. The user needs to know the app and the server disagree
     * while the task is still on screen; silently keeping the app's belief is what made
     * the 4.6.0 and 4.7.0 response-shape changes look like bugs in the app.
     */
    private fun checkContract(
        task: ServerTask,
        sent: TaskContractSent,
        expectedId: String? = null,
    ) {
        val mismatched = contractMismatches(sent, task, expectedId)
        if (mismatched.isEmpty()) return
        // Not thrown. The write succeeded — the task IS stored — so failing it would
        // tell the user their work was lost when it was not, and they would enter it
        // again. Reporting an outcome that did not happen is the exact class of lie
        // #214 is about, so this warns and leaves the stored task authoritative.
        ContractWarnings.report(mismatched, task.name)
    }

    /**
     * The version half of [checkContract], for the responses that carry no
     * request to compare against (#185).
     *
     * `complete` and `reopen` send no fields — there is nothing to assert —
     * but the issue's evidence is exactly a complete-response shape change
     * that parsed cleanly and read as a rollover bug. The version is the only
     * thing those paths CAN assert, so they do. Same warn-not-throw channel:
     * the mutation succeeded, only the contract is in doubt.
     *
     * `list` and `get` deliberately do not call this: a list refresh would
     * re-warn on every poll against one old server, and a get fires from
     * background flows with no screen to show it on. Writes are the moments
     * the user is looking.
     */
    private fun checkVersion(task: ServerTask) {
        if (task.contractVersion != TASK_CONTRACT_VERSION) {
            ContractWarnings.report(listOf("contract_version"), task.name)
        }
    }

    private fun parseOne(body: String): ServerTask =
        parseTask(JSONObject(body))
            ?: throw RuntimeException(TASKS_SHAPE_ERROR)

    /** Ids are hex today, but encoded defensively so a malformed id can
     * never break the request path (the `+` fix-up mirrors ServerApi:
     * form-encoding emits `+` for space, valid only in query strings). */
    private fun encodeId(id: String): String =
        URLEncoder.encode(id.trim(), Charsets.UTF_8.name()).replace("+", "%20")

    /**
     * A 404 naming the row means the row is gone regardless of verb: a
     * single-verb request URL already identifies which row (it carries
     * exactly one id), so the body match alone is the eviction signal —
     * or About names a reset for a task that no longer exists (#480
     * review). Batch callers pass only ids the message itself names (a
     * batch URL names no row).
     */
    private fun clearSingleIfGone(id: String, msg: String) {
        if ("HTTP 404:" in msg && "unknown task" in msg) {
            noteCoercedNotes("single", singleCoercionState(id), emptyList())
        }
    }

    private fun call(method: String, url: String, body: JSONObject? = null): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
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
                    // Server URL points at the gateway (:8643) instead of the
                    // proxy (:8080) — the gateway serves chat only, while
                    // /api/* lives on health-api behind the proxy. Say so
                    // instead of surfacing a bare "Not Found".
                    if (response.code == 404 && url.startsWith("/api/")) {
                        throw RuntimeException(
                            "HTTP 404: ${text.take(200)} (is the Server URL " +
                                "the :8080 proxy? Direct :8643 serves chat only)"
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
    /** Longest task name shown in the warning before it is elided. */
    const val NAME_DISPLAY_MAX = 40

    private val _mismatched = MutableStateFlow<ContractMismatch?>(null)

    /**
     * Newest mismatch, or null when there is nothing pending. Null rather than an empty
     * list so "nothing to show" and "a mismatch with no fields" cannot be confused —
     * [report] rejects the latter anyway, but the type should not have to carry that.
     *
     * NEWEST WINS, deliberately. `StateFlow` holds one value, so three reports arriving
     * while a single toast is up coalesce to the last one. That is a policy rather
     * than a second loss path: the pending warnings all describe the same underlying
     * disagreement with the same server, and stacking three identical toasts in a
     * queue the user must dismiss before reaching the conversation would be worse than
     * showing the most recent one. What must never happen is the newest being WIPED by
     * the cleanup of an older one — hence [consume] matching on the generation.
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
     * increment would let one warning clear another — which is why [report] increments and
     * stores under one lock rather than as two steps.
     */
    private val _generation = AtomicInteger(0)

    /** The current generation. Exposed for tests and diagnostics. */
    val generation: Int get() = _generation.get()

    /**
     * Guards the increment-and-set below. Private rather than locking on `this`, which is
     * a public singleton, and rather than synchronising the object so nothing outside can
     * lock it by accident.
     */
    private val reportLock = Any()

    fun report(fields: List<String>, taskName: String = "") {
        if (fields.isEmpty()) return
        // Increment and set together, under one lock.
        //
        // As two operations they can reorder across threads: A takes gen5, B takes gen6, B
        // writes first, A writes last — so the stale gen5 lands last and the NEWER warning
        // is the one that disappears. `report()` runs on Dispatchers.IO and two concurrent
        // autosaves are ordinary, not exotic.
        //
        // The lock is uncontended in practice (one write per mismatch, not per frame), so
        // it costs nothing; the alternative — a compare-and-set retry loop — is more code
        // for the same guarantee and harder to read.
        synchronized(reportLock) {
            _mismatched.value =
                ContractMismatch(_generation.incrementAndGet(), fields, taskName)
        }
    }

    /**
     * Clears the warning **only if it is still the one that was shown**.
     *
     * [shown] is the generation passed to [message], not the field list, and that
     * distinction is the whole fix.
     *
     * The first version compared the LISTS:
     *
     *     collect[A] -> toast(A)           // returns at once; a second report
     *     report(B)  -> value = [B]          // can still land while it shows
     *     consume(A) -> clears iff value == A
     *
     * which holds only while A != B. Two reports of the SAME fields break it:
     * `report([X])`, `toast([X])`, `report([X])`, `consume([X])` clears the second
     * one — and same-fields is the COMMON case, not the exotic one, because both writes
     * are hitting the same broken server and get the same answer from it.
     *
     * Matching on a monotonic generation instead makes the two reports distinct values,
     * so clearing the first cannot touch the second however alike they look.
     */
    fun consume(shown: Int) {
        val current = _mismatched.value ?: return
        if (current.generation != shown) return
        // CAS, not a plain assignment. The read above and the write here are two
        // operations, and a `report()` can land BETWEEN them: the check passes
        // against gen1, the new gen2 is stored, and the assignment then wipes
        // it — the exact lost-warning shape the generation exists to prevent,
        // with the window narrowed rather than closed.
        //
        // `compareAndSet` only writes if the value is still `current`, so a report that
        // arrived in the meantime survives and is collected next.
        _mismatched.compareAndSet(current, null)
    }

    /**
     * The user-facing text. Names the fields: "something differs" sends them hunting.
     *
     * Deliberately does not say "reopen the task" — `reopen` is a real verb in this API
     * meaning the opposite (it un-completes a task), so telling a user to reopen a task
     * that is already saved reads as an instruction to undo it.
     *
     * It asks the user to RELOAD rather than claiming a reload happened. The collector
     * does bump `refreshTick`, which starts an async `api.list` — and that load can fail,
     * offline being the obvious case. "The list is refreshing" would be a claim about
     * something that may not have occurred, sitting on top of the stale row the user was
     * trying to fix. Asking keeps the sentence true either way.
     */
    fun message(fields: List<String>, taskName: String = ""): String {
        // Name the task. Without it the warning is not actionable on a list screen:
        // "Saved, but the server stored different values for due_date" could belong to any
        // row, and the user has to guess which one to open. Autosave makes that worse —
        // several edits can land between the toast and the user's attention.
        // Truncate for DISPLAY only; the full name stays in the record. A 200-character
        // task name would push the field list — the actionable half — off the toast
        // card, which is the opposite of what the longer text bought.
        val shown = taskName.trim().let { if (it.length > NAME_DISPLAY_MAX) it.take(NAME_DISPLAY_MAX - 1) + "\u2026" else it }
        val forWhich = shown.takeIf { it.isNotEmpty() }?.let { " for \u201c$it\u201d" }.orEmpty()
        return "Saved, but the server stored different values$forWhich: " +
            fields.joinToString(", ") +
            ". Your app and the server may be on different versions — reload the " +
            "list to see what was actually stored."
    }
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
    /** Name of the task the mismatch is about; "" when unknown. */
    val taskName: String = "",
)
