package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
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
    val completedAt: String = "",
    val createdAt: String = "",
    /**
     * The next occurrence the server created by itself when a structured
     * cadence was completed ("" when there is none, or when the repeat is a
     * custom condition the caller has to schedule itself).
     */
    val nextDueDate: String = "",
)

/**
 * The moment the work is meant to begin: `due - estimated_minutes`, which
 * is exactly when the "Start now" reminder fires. A zero estimate has no
 * start nudge, so the due time is used instead — the same rule the server's
 * reminder engine applies, so the two can never disagree about when a task
 * starts. Null when the task has no usable due time.
 */
fun ServerTask.dueMillisOrNull(): Long? = dueMillisOrNull(dueDate, dueTime)

fun ServerTask.startMillisOrNull(): Long? {
    val due = dueMillisOrNull() ?: return null
    return if (estimatedMinutes > 0) {
        due - estimatedMinutes * 60_000L
    } else {
        due
    }
}

/** Start moment as the (date, HH:mm) pair the display helpers take. */
fun ServerTask.startParts(): Pair<String, String>? {
    val at = startMillisOrNull() ?: return null
    val ldt = java.time.Instant.ofEpochMilli(at).atZone(IST).toLocalDateTime()
    return ldt.toLocalDate().toString() to
        String.format(Locale.ROOT, "%02d:%02d", ldt.hour, ldt.minute)
}

/**
 * "Today, 07:00 → 09:00" — the start and the deadline on one line. Within
 * a day only the second time is repeated; when the start crosses midnight
 * (due 00:30, 1h estimate) both sides keep their own day, because a bare
 * "23:30 → 00:30" reads as nonsense.
 */
fun ServerTask.startToDueLine(today: java.time.LocalDate): String {
    val due = friendlyDue(dueDate, dueTime, today)
    val parts = startParts() ?: return due
    val start = friendlyDue(parts.first, parts.second, today)
    if (start.isEmpty() || due.isEmpty()) return due.ifEmpty { start }
    // Same day: the day is already stated by the start side, so only the
    // due time is repeated. Compared on the date parts, not by stripping
    // text off a formatted line.
    if (parts.first == dueDate) {
        // Formatted from the parsed instant, not the raw string, so a
        // stored "9:00" cannot render unpadded next to a zero-padded start.
        val at = dueMillisOrNull() ?: return start
        val d = java.time.Instant.ofEpochMilli(at).atZone(IST).toLocalDateTime()
        val time = String.format(Locale.ROOT, "%02d:%02d", d.hour, d.minute)
        return "$start → $time"
    }
    return "$start → $due"
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
        // One shared client: each OkHttpClient owns a connection pool and
        // dispatcher threads, so per-call instances would leak both.
        private val sharedHttp: OkHttpClient by lazy {
            OkHttpClient.Builder()
                .connectTimeout(10, TimeUnit.SECONDS)
                .readTimeout(30, TimeUnit.SECONDS)
                .writeTimeout(30, TimeUnit.SECONDS)
                .build()
        }
    }

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http: OkHttpClient get() = sharedHttp

    private fun base(): String {
        for (k in listOf("server_base_url", "server_url", "api_base_url")) {
            val v = (prefs.getString(k, "") ?: "").trim().trimEnd('/')
            if (v.isNotEmpty()) return v
        }
        return ""
    }

    private fun password(): String {
        val v = (prefs.getString("app_password", "") ?: "").trim()
        if (v.isNotEmpty()) return v
        return (prefs.getString("auth_token", "") ?: "").trim()
    }

    /** Lenient string read: Android's `optString` coerces JSON null to
     * the string "null", so every nullable/completedAt-style field must
     * go through here (#130). */
    private fun optStr(o: JSONObject, key: String): String =
        if (o.isNull(key)) "" else o.optString(key, "").trim()

    private fun parseTask(o: JSONObject): ServerTask? {
        val id = optStr(o, "id")
            .ifEmpty { optStr(o, "_id") }
        if (id.isEmpty()) return null
        val name = optStr(o, "name")
        if (name.isEmpty()) return null
        // estimated_minutes may encode as int, long, or double.
        val mins = (o.opt("estimated_minutes") as? Number)?.toInt() ?: 0
        return ServerTask(
            id = id,
            name = name,
            description = optStr(o, "description"),
            dueDate = optStr(o, "due_date"),
            dueTime = optStr(o, "due_time"),
            estimatedMinutes = mins.coerceAtLeast(0),
            repeatEvery = (o.opt("repeat_every") as? Number)?.toInt() ?: 0,
            repeatUnit = optStr(o, "repeat_unit"),
            repeatCustom = o.optBoolean("repeat_custom", false),
            repeatRule = optStr(o, "repeat_rule"),
            parallelable = o.optBoolean("parallelable", false),
            completedAt = optStr(o, "completedAt"),
            createdAt = optStr(o, "createdAt"),
        )
    }

    /** Lists tasks by state (`open`, `done`, `all`); unknown states fail fast. */
    suspend fun list(state: String = "open"): Result<List<ServerTask>> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val s = state.trim().lowercase(Locale.ROOT)
            if (s != "open" && s != "done" && s != "all") {
                return@withContext Result.failure(IllegalArgumentException("Unknown state: $state"))
            }
            call("GET", "/api/tasks?state=$s").map { body ->
                val out = mutableListOf<ServerTask>()
                val root = JSONObject(body)
                val arr = root.optJSONArray("tasks")
                    ?: root.optJSONArray("data")
                    ?: root.optJSONArray("items")
                    ?: return@map out
                for (i in 0 until arr.length()) {
                    val o = arr.optJSONObject(i) ?: continue
                    parseTask(o)?.let { out.add(it) }
                }
                out
            }
        }

    /** Creates a task. Everything except the repeat is required — the
     * server rejects missing keys (all-zero repeat = one-shot). The
     * repeat is either structured or custom, never both. */
    suspend fun create(
        name: String,
        description: String,
        dueDate: String,
        dueTime: String,
        estimatedMinutes: Int,
        repeatEvery: Int = 0,
        repeatUnit: String = "",
        repeatCustom: Boolean = false,
        repeatRule: String = "",
        parallelable: Boolean = false,
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
        val body = JSONObject()
            .put("name", name.trim())
            .put("description", description.trim())
            .put("due_date", dueDate.trim())
            .put("due_time", dueTime.trim())
            .put("estimated_minutes", estimatedMinutes)
            .put("parallelable", parallelable)
            .put("repeat_every", repeatEvery)
            .put("repeat_unit", repeatUnit)
            .put("repeat_custom", repeatCustom)
            .put("repeat_rule", repeatRule.trim())
        call("POST", "/api/tasks", body).map { parseOne(it) }
    }

    /** Partial edit: only non-null keys are sent. The repeat keys are sent
     * together or not at all, so the server can validate the recurrence as
     * a whole instead of merging half of it. */
    suspend fun update(
        id: String,
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
    ): Result<ServerTask> = withContext(Dispatchers.IO) {
        val clean = encodeId(id)
        if (clean.isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Missing task id"))
        }
        val body = JSONObject()
        if (name != null) body.put("name", name)
        if (description != null) body.put("description", description)
        if (dueDate != null) body.put("due_date", dueDate)
        if (dueTime != null) body.put("due_time", dueTime)
        if (estimatedMinutes != null) body.put("estimated_minutes", estimatedMinutes)
        if (repeatEvery != null) body.put("repeat_every", repeatEvery)
        if (repeatUnit != null) body.put("repeat_unit", repeatUnit)
        if (repeatCustom != null) body.put("repeat_custom", repeatCustom)
        if (repeatRule != null) body.put("repeat_rule", repeatRule)
        if (parallelable != null) body.put("parallelable", parallelable)
        call("PATCH", "/api/tasks/$clean", body).map { parseOne(it) }
    }

    /** Marks a task done (server starts the 3-day retention clock). The
     * response carries the rolled-over occurrence in `next` when a
     * structured cadence produced one. */
    suspend fun complete(id: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("POST", "/api/tasks/$clean/complete", JSONObject()).map { body ->
                val root = JSONObject(body)
                val task = parseTask(root.optJSONObject("task") ?: root)
                    ?: throw RuntimeException("Unexpected response shape")
                val next = root.optJSONObject("next")
                if (next == null) {
                    task
                } else {
                    // optStr, not optString: a JSON null would arrive as the
                    // literal string "null" and reach the snackbar.
                    task.copy(nextDueDate = optStr(next, "due_date"))
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
        }

    /** Permanently deletes a task. */
    suspend fun delete(id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("DELETE", "/api/tasks/$clean").map { }
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
