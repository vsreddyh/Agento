package com.vishnu.agento

import android.content.Context
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONArray
import org.json.JSONObject
import java.net.URLEncoder
import java.util.concurrent.TimeUnit

/** One resumable agent run from the gateway Runs API (`/api/runs`, #311). */
data class AgentRun(
    val id: String,
    val profile: String = "",
    val status: String = "",
    val conversationId: String = "",
    /** Stored prompt preview (bounded server-side), for matching a resend. */
    val prompt: String = "",
    /** Accumulated output, or the list preview when truncated. */
    val output: String = "",
    val reasoning: String = "",
    val error: String = "",
    val seq: Long = 0L,
    val model: String = "",
    val tools: List<String> = emptyList(),
    val createdAt: Long = 0L,
    val updatedAt: Long = 0L,
    val finishedAt: Long = 0L,
    /** Server-reported token counts for the turn (null = unreported). */
    val usage: TokenUsage? = null,
) {
    /** True for `completed|failed|cancelled|interrupted` — nothing more runs.
     * Liveness decisions everywhere use this, never a status equality. */
    fun isTerminal(): Boolean = status == "completed" || status == "failed" ||
        status == "cancelled" || status == "interrupted"
}

/** Outcome of `POST /api/runs/{id}/steer`. */
sealed interface SteerOutcome {
    /** The instruction reached the running turn. */
    data object Queued : SteerOutcome
    /** The run settled first (HTTP 409): steer the next turn instead. */
    data class Conflict(val message: String) : SteerOutcome
}

/**
 * Client over the gateway's native Runs API (same bearer auth as chat,
 * served through the proxy's /p/ route like the Jobs API — always call with
 * the profile path, e.g. `/p/chat`, since an unprefixed `/api/` path belongs
 * to health-api). Parsing is lenient across gateway versions; missing fields
 * show as blank rather than failing the whole list.
 */
class RunsApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        /** Query/path encoding matching ProjectsApi/TasksApi (`+` is a space
         * in form encoding, not in a path — `%20` keeps ids exact). */
        internal fun encode(v: String): String =
            URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")
    }

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

    private data class CallResult(val code: Int, val body: String)

    private fun call(method: String, url: String, body: JSONObject? = null): Result<CallResult> {
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
                "POST" -> builder.post(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
                else -> throw IllegalArgumentException("unknown method $method")
            }
            http.newCall(builder.build()).execute().use { response ->
                CallResult(response.code, response.body?.string() ?: "")
            }
        }
    }

    private fun parseList(body: String): List<AgentRun> {
        val root = runCatching { JSONObject(body) }.getOrNull()
        if (root == null) {
            runCatching { Log.w("RunsApi", "unparseable runs body: ${body.take(120)}") }
            return emptyList()
        }
        val arr = root.optJSONArray("runs")
        if (arr == null) {
            runCatching { Log.w("RunsApi", "runs body has no runs array: ${body.take(120)}") }
            return emptyList()
        }
        val (out, dropped) = parseAgentRunArray(arr)
        if (dropped > 0) {
            runCatching { Log.w("RunsApi", "dropped $dropped id-less run row(s)") }
        }
        return out
    }

    /** Lists runs, newest first. Null filters mean "no constraint". */
    suspend fun list(
        path: String,
        conversationId: String? = null,
        status: String? = null,
        limit: Int = 50,
    ): Result<List<AgentRun>> = withContext(Dispatchers.IO) {
        var url = "$path/api/runs?limit=$limit"
        if (!conversationId.isNullOrBlank()) url += "&conversation_id=${encode(conversationId.trim())}"
        if (!status.isNullOrBlank()) url += "&status=${encode(status.trim())}"
        // Any non-200 is an error, not an empty list: a 401/500 rendering as
        // "no runs yet" would hide an outage behind a plausible empty state.
        call("GET", url).mapCatching {
            if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            parseList(it.body)
        }
    }

    /**
     * Queues an instruction into the running turn. Returns [SteerOutcome.Conflict]
     * when the run settled first (HTTP 409) — steer the next turn instead.
     */
    suspend fun steer(path: String, id: String, message: String): Result<SteerOutcome> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/runs/${encode(id)}/steer", JSONObject().put("message", message))
                .mapCatching {
                    when (it.code) {
                        200 -> SteerOutcome.Queued
                        409 -> SteerOutcome.Conflict(it.body.take(200))
                        else -> throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
                    }
                }
        }

    /**
     * Aborts the run, keeping partial output. Success only acknowledges —
     * the run's own status (via streams/records) is the source of truth.
     */
    suspend fun cancel(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/runs/${encode(id)}/cancel").mapCatching {
                if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            }
        }

    /**
     * Fetches one run by id (#374): the live snapshot when it is running,
     * else the stored record. 404 (unknown id) is a failure, not a null —
     * the caller treats any failure as "no match" and leaves the tail.
     */
    suspend fun get(path: String, id: String): Result<AgentRun> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/runs/${encode(id)}").mapCatching {
                if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
                // Null (id-less row) and malformed JSON both read as
                // "unparseable": either way there is no adoptable record,
                // and the message says which half failed.
                val parsed = runCatching { JSONObject(it.body) }.getOrNull()
                    ?: throw RuntimeException("run record is not JSON: ${it.body.take(120)}")
                parseAgentRun(parsed)
                    ?: throw RuntimeException("run record has no id: ${it.body.take(120)}")
            }
        }

}

/** Pure parse of one run record (#374): the detail shape and every list
 * row share it, so the adopt path reads exactly what the sheet shows.
 * Null for id-less rows — they can never be adopted or cancelled. */
fun parseAgentRun(o: JSONObject): AgentRun? {
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    val tools = buildList {
        val arr = o.optJSONArray("tools")
        if (arr != null) {
            for (i in 0 until arr.length()) add(arr.optString(i, ""))
        }
    }.filter { it.isNotBlank() }
    return AgentRun(
        id = id,
        profile = o.optString("profile", "").trim(),
        status = o.optString("status", "").trim(),
        conversationId = o.optString("conversation_id", "").trim(),
        prompt = o.optString("prompt", "").trim(),
        output = o.optString("output", ""),
        reasoning = o.optString("reasoning", ""),
        error = o.optString("error", "").trim(),
        seq = o.optLong("seq", 0L),
        model = o.optString("model", "").trim(),
        tools = tools,
        createdAt = o.optLong("created_at", 0L),
        updatedAt = o.optLong("updated_at", 0L),
        finishedAt = o.optLong("finished_at", 0L),
        usage = runCatching { parseTokenUsage(o) }.getOrNull(),
    )
}

/** Pure parse of the runs list body: unparseable is empty, never an error
 * (the callers decide whether empty means "none" or "outage"). Shares the
 * array parse with production below, so tests pin what prod runs. */
fun parseAgentRunList(body: String): List<AgentRun> {
    val root = runCatching { JSONObject(body) }.getOrNull() ?: return emptyList()
    val arr = root.optJSONArray("runs") ?: return emptyList()
    return parseAgentRunArray(arr).first
}

/**
 * Pure parse of one runs-list array: rows plus the id-less drop count the
 * production wrapper logs. The single row parser both paths share, so a
 * drift in null-row handling breaks the pinned tests instead of the app.
 */
fun parseAgentRunArray(arr: JSONArray): Pair<List<AgentRun>, Int> {
    val out = mutableListOf<AgentRun>()
    var dropped = 0
    for (i in 0 until arr.length()) {
        // Non-object rows (a bare string, a null) are the same corruption
        // class as id-less objects: skip and count them, so the log
        // names every shape the backup refused.
        val o = arr.optJSONObject(i)
        if (o == null) {
            dropped++
            continue
        }
        val run = parseAgentRun(o)
        if (run == null) {
            dropped++
            continue
        }
        out.add(run)
    }
    return out to dropped
}
