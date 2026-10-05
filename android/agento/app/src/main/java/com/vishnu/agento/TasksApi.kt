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
            // Missing on pre-revision servers: 0 matches the backfill, so
            // an old server and a new app still agree (#184).
            revision = (o.opt("revision") as? Number)?.toInt() ?: 0,
            completedAt = optStr(o, "completedAt"),
            createdAt = optStr(o, "createdAt"),
        )
    }

    /**
     * Lists tasks by state (`open`, `done`, `all`); unknown states fail fast.
     *
     * One bounded page: [limit] <= 0 takes the server default (200, max
     * 500), and [TaskList.truncated] says whether more rows exist, so the
     * list never silently ends mid-collection (#168).
     */
    suspend fun list(state: String = "open", limit: Int = 0): Result<TaskList> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val s = state.trim().lowercase(Locale.ROOT)
            if (s != "open" && s != "done" && s != "all") {
                return@withContext Result.failure(IllegalArgumentException("Unknown state: $state"))
            }
            val query = if (limit > 0) "/api/tasks?state=$s&limit=$limit" else "/api/tasks?state=$s"
            call("GET", query).map { body ->
                val out = mutableListOf<ServerTask>()
                val root = JSONObject(body)
                val arr = root.optJSONArray("tasks")
                    ?: root.optJSONArray("data")
                    ?: root.optJSONArray("items")
                    ?: return@map TaskList(out)
                for (i in 0 until arr.length()) {
                    val o = arr.optJSONObject(i) ?: continue
                    parseTask(o)?.let { out.add(it) }
                }
                TaskList(out, root.optBoolean("truncated", false))
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
                    Result.success(task)
                },
                onFailure = { e ->
                    // Code and body: a gateway 404 (wrong server) must never
                    // read as a finished task.
                    val msg = e.message ?: ""
                    if ("HTTP 404:" in msg && "unknown task" in msg) {
                        Result.success(null)
                    } else {
                        Result.failure(e)
                    }
                },
            )
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
        // #185: trimmed once, and the SAME values are asserted and sent. Trimming only
        // in the comparison would leave the two able to disagree on padding, which the
        // trim-insensitive compare then hides — so the app would depend on the server
        // cleaning up before they agree.
        val sent = buildTaskContractSent(
            name = name,
            description = description,
            dueDate = dueDate,
            dueTime = dueTime,
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit,
            repeatCustom = repeatCustom,
            repeatRule = repeatRule,
            parallelable = parallelable,
        )
        // The body reads its strings back OUT of `sent`: two trims are two rules, and the
        // one that drifts is the one the trim-insensitive compare hides. `sent` is
        // all-non-null here because create validates every field above.
        // `requireNotNull` on every field, deliberately.
        //
        // `create` takes NON-NULL parameters and validates the four required strings as
        // non-blank above, so `sent.*` cannot be null here — the builder is only nullable
        // because `update` shares it. But `JSONObject.put(String, Object?)` given a null
        // silently REMOVES the key rather than storing a null, so a future relaxation of
        // those validations would turn a create into a request missing `name`, and the
        // server's answer would be a confusing "field required" rather than a local
        // failure naming the cause.
        //
        // These make the invariant FAIL LOUDLY at the point it breaks. They are not
        // defensive noise: each one pins "create asserts every field it sends", which is
        // exactly what the wiring test asserts.
        // Fail loud AND stay in-channel.
        //
        // These ten cannot be null — `create` takes non-null parameters and validates the
        // four required strings as non-blank above — so this is a guard against a future
        // relaxation, not a runtime path. `JSONObject.put(String, Object?)` given a null
        // silently REMOVES the key, so a relaxed validation would become a create missing
        // `name` and a server-side "field required" instead of a local failure.
        //
        // It returns `Result.failure` rather than throwing. Callers do
        // `api.create(...).fold(onSuccess = …, onFailure = ::fail)` inside `scope.launch`
        // with no try/catch, so an exception thrown out of this suspend would escape the
        // coroutine and NO SNACKBAR WOULD APPEAR — the exact silent-failure shape this
        // whole PR exists to remove. An earlier version used `requireNotNull` and
        // reintroduced it.
        val missing = buildList {
            if (sent.name == null) add("name")
            if (sent.description == null) add("description")
            if (sent.dueDate == null) add("due_date")
            if (sent.dueTime == null) add("due_time")
            if (sent.estimatedMinutes == null) add("estimated_minutes")
            if (sent.parallelable == null) add("parallelable")
            if (sent.repeatEvery == null) add("repeat_every")
            if (sent.repeatUnit == null) add("repeat_unit")
            if (sent.repeatCustom == null) add("repeat_custom")
            if (sent.repeatRule == null) add("repeat_rule")
        }
        if (missing.isNotEmpty()) {
            return@withContext Result.failure(
                IllegalStateException(
                    "create: builder dropped ${missing.joinToString()}; " +
                        "create asserts every field it sends",
                ),
            )
        }
        val body = JSONObject()
            .put("name", sent.name)
            .put("description", sent.description)
            .put("due_date", sent.dueDate)
            .put("due_time", sent.dueTime)
            .put("estimated_minutes", sent.estimatedMinutes)
            .put("parallelable", sent.parallelable)
            .put("repeat_every", sent.repeatEvery)
            .put("repeat_unit", sent.repeatUnit)
            .put("repeat_custom", sent.repeatCustom)
            .put("repeat_rule", sent.repeatRule)
        call("POST", "/api/tasks", body).map { parseOne(it) }
            .also { r -> r.getOrNull()?.let { checkContract(it, sent) } }
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
     */
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
            description = description,
            dueDate = dueDate,
            dueTime = dueTime,
            estimatedMinutes = estimatedMinutes,
            repeatEvery = repeatEvery,
            repeatUnit = repeatUnit,
            repeatCustom = repeatCustom,
            repeatRule = repeatRule,
            parallelable = parallelable,
        )
        val body = JSONObject()
        sent.name?.let { body.put("name", it) }
        sent.description?.let { body.put("description", it) }
        sent.dueDate?.let { body.put("due_date", it) }
        sent.dueTime?.let { body.put("due_time", it) }
        sent.estimatedMinutes?.let { body.put("estimated_minutes", it) }
        sent.repeatEvery?.let { body.put("repeat_every", it) }
        sent.repeatUnit?.let { body.put("repeat_unit", it) }
        sent.repeatCustom?.let { body.put("repeat_custom", it) }
        sent.repeatRule?.let { body.put("repeat_rule", it) }
        sent.parallelable?.let { body.put("parallelable", it) }
        if (expectedRevision != null) body.put("expected_revision", expectedRevision)
        call("PATCH", "/api/tasks/$clean", body).map { parseOne(it) }
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
                    checkContract(task, sent)
                }
            }
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

    /**
     * Compares a write's response against what was asked for, and surfaces any field
     * the server stored differently.
     *
     * A snackbar, not an exception: the write genuinely succeeded, so failing it would
     * tell the user their task was not saved when it was. That is the same class of lie
     * as #214 — reporting an outcome that did not happen — and it is the reason this
     * warns instead of throws. The user needs to know the app and the server disagree
     * while the task is still on screen; silently keeping the app's belief is what made
     * the 4.6.0 and 4.7.0 response-shape changes look like bugs in the app.
     */
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
     *
     * NEWEST WINS, deliberately. `StateFlow` holds one value, so three reports arriving
     * while a single snackbar is up coalesce to the last one. That is a policy rather
     * than a second loss path: the pending warnings all describe the same underlying
     * disagreement with the same server, and stacking three identical snackbars in a
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

    fun report(fields: List<String>) {
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
            _mismatched.value = ContractMismatch(_generation.incrementAndGet(), fields)
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
        if (current.generation != shown) return
        // CAS, not a plain assignment. The read above and the write here are two
        // operations, and `showSnackbar` suspends for its whole duration, so a
        // `report()` can land BETWEEN them: the check passes against gen1, the new gen2
        // is stored, and the assignment then wipes it — the exact lost-warning shape the
        // generation exists to prevent, with the window narrowed rather than closed.
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
    fun message(fields: List<String>): String =
        "Saved, but the server stored different values for " +
            fields.joinToString(", ") +
            ". Your app and the server may be on different versions — reload the " +
            "list to see what was actually stored."
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
