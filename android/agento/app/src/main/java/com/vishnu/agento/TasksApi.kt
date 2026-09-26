package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
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
    val repeatRule: String = "",
    val completedAt: String = "",
    val createdAt: String = "",
)

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

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

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

    private fun parseTask(o: JSONObject): ServerTask? {
        val id = o.optString("id", "")
            .ifEmpty { o.optString("_id", "") }.trim()
        if (id.isEmpty()) return null
        val name = o.optString("name", "").trim()
        if (name.isEmpty()) return null
        // estimated_minutes may encode as int, long, or double.
        val mins = (o.opt("estimated_minutes") as? Number)?.toInt() ?: 0
        return ServerTask(
            id = id,
            name = name,
            description = o.optString("description", "").trim(),
            dueDate = o.optString("due_date", "").trim(),
            dueTime = o.optString("due_time", "").trim(),
            estimatedMinutes = mins.coerceAtLeast(0),
            repeatRule = o.optString("repeat_rule", "").trim(),
            completedAt = o.optString("completedAt", "").trim(),
            createdAt = o.optString("createdAt", "").trim(),
        )
    }

    /** Lists tasks by state (`open`, `done`, `all`); unknown states fail fast. */
    suspend fun list(state: String = "open"): Result<List<ServerTask>> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val s = state.trim().lowercase()
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

    /** Fetches one task by id. */
    suspend fun get(id: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = id.trim()
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("GET", "/api/tasks/$clean").map { parseOne(it) }
        }

    /** Creates an open task; name required, the rest validated server-side. */
    suspend fun create(
        name: String,
        description: String = "",
        dueDate: String = "",
        dueTime: String = "",
        estimatedMinutes: Int? = null,
        repeatRule: String = "",
    ): Result<ServerTask> = withContext(Dispatchers.IO) {
        if (name.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Name is required"))
        }
        val body = JSONObject()
            .put("name", name.trim())
            .put("description", description)
            .put("due_date", dueDate)
            .put("due_time", dueTime)
            .put("repeat_rule", repeatRule)
        if (estimatedMinutes != null) body.put("estimated_minutes", estimatedMinutes)
        call("POST", "/api/tasks", body).map { parseOne(it) }
    }

    /** Partial edit: only non-null keys are sent (empty string clears
     * due/repeat fields server-side). */
    suspend fun update(
        id: String,
        name: String? = null,
        description: String? = null,
        dueDate: String? = null,
        dueTime: String? = null,
        estimatedMinutes: Int? = null,
        repeatRule: String? = null,
    ): Result<ServerTask> = withContext(Dispatchers.IO) {
        val clean = id.trim()
        if (clean.isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Missing task id"))
        }
        val body = JSONObject()
        if (name != null) body.put("name", name)
        if (description != null) body.put("description", description)
        if (dueDate != null) body.put("due_date", dueDate)
        if (dueTime != null) body.put("due_time", dueTime)
        if (estimatedMinutes != null) body.put("estimated_minutes", estimatedMinutes)
        if (repeatRule != null) body.put("repeat_rule", repeatRule)
        call("PATCH", "/api/tasks/$clean", body).map { parseOne(it) }
    }

    /** Marks a task done (server starts the 3-day retention clock). */
    suspend fun complete(id: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = id.trim()
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("POST", "/api/tasks/$clean/complete", JSONObject()).map { parseOne(it) }
        }

    /** Reopens a done task. */
    suspend fun reopen(id: String): Result<ServerTask> =
        withContext(Dispatchers.IO) {
            val clean = id.trim()
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("POST", "/api/tasks/$clean/reopen", JSONObject()).map { parseOne(it) }
        }

    /** Permanently deletes a task. */
    suspend fun delete(id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            val clean = id.trim()
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing task id"))
            }
            call("DELETE", "/api/tasks/$clean").map { }
        }

    private fun parseOne(body: String): ServerTask =
        parseTask(JSONObject(body))
            ?: throw RuntimeException("Unexpected response shape")

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
                else -> builder.get()
            }
            http.newCall(builder.build()).execute().use { response ->
                val text = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }
}

/** Pulls the server's `{"detail": "..."}` message out of an HTTP error so
 * validation failures (bad due date, unknown id) read as plain sentences. */
fun serverDetail(message: String): String {
    val m = Regex(""""detail"\s*:\s*"((?:[^"\\]|\\.)*)"""").find(message)
    val detail = m?.groupValues?.getOrNull(1)
        ?.replace("\\\"", "\"")
        ?.replace("\\\\", "\\")
        ?.trim().orEmpty()
    return detail.ifEmpty { message }
}
