package com.vishnu.agento

import android.content.Context
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/** One scheduled job from the gateway Jobs API (`/api/jobs`). */
data class CronJob(
    val id: String,
    val name: String = "",
    val prompt: String = "",
    val schedule: String = "",
    /** True = paused; null when the server doesn't report state. */
    val paused: Boolean? = null,
    val lastRun: String = "",
    val nextRun: String = "",
    val delivery: String = "",
    /** Short stored history, oldest first (capped server-side). */
    val runs: List<CronRun> = emptyList(),
)

/** One firing of a cron job: when it ran and what the model said. */
data class CronRun(
    /** Fire instant, Unix millis. 0 when the server doesn't report it. */
    val at: Long = 0L,
    val text: String = "",
    val missed: Boolean = false,
)

/** A job-list page: the rows, and how many rows arrived unparseable
 * (non-object rows or rows without a usable id) and were skipped rather
 * than shown (#430). A nonzero [dropped] means the server spoke rows
 * this app cannot read. */
data class JobList(
    val jobs: List<CronJob>,
    val dropped: Int = 0,
)

/** Presence that treats explicit JSON null as absent (#430): `has`
 * alone is true for a null value, which would read a null tri-state as
 * a value instead of unknown. */
internal fun hasValue(o: JSONObject, key: String): Boolean =
    o.has(key) && !o.isNull(key)

/** One job row: null when the row carries no usable id.
 * Every string reads through [optJsonStr]: a bare `optString` would
 * coerce JSON null to the literal string "null", surfacing phantom
 * rows and "null" text (#430). */
internal fun parseJob(o: JSONObject): CronJob? {
    val id = optJsonStr(o, "id")
        .ifEmpty { optJsonStr(o, "job_id") }
    if (id.isEmpty()) return null
    val paused = when {
        hasValue(o, "paused") -> o.optBoolean("paused")
        hasValue(o, "enabled") -> !o.optBoolean("enabled", true)
        hasValue(o, "state") -> optJsonStr(o, "state").lowercase() == "paused"
        hasValue(o, "status") -> optJsonStr(o, "status").lowercase() == "paused"
        else -> null
    }
    return CronJob(
        id = id,
        name = optJsonStr(o, "name"),
        prompt = optJsonStr(o, "prompt"),
        schedule = optJsonStr(o, "schedule")
            .ifEmpty { optJsonStr(o, "cron") },
        paused = paused,
        lastRun = optJsonStr(o, "last_run")
            .ifEmpty { optJsonStr(o, "lastRun") },
        nextRun = optJsonStr(o, "next_run")
            .ifEmpty { optJsonStr(o, "nextRun") },
        delivery = optJsonStr(o, "delivery")
            .ifEmpty { optJsonStr(o, "deliver") },
        runs = o.optJSONArray("runs")?.let { arr ->
            buildList {
                for (i in 0 until arr.length()) {
                    val r = arr.optJSONObject(i) ?: continue
                    add(
                        CronRun(
                            at = r.optLong("at", 0L),
                            text = optJsonStr(r, "text"),
                            missed = r.optBoolean("missed", false),
                        )
                    )
                }
            }
        }.orEmpty(),
    )
}

/** Parses one job-list page (#430): the `jobs`/`data`/`items` envelope,
 * the rows, and how many rows arrived unparseable.
 *
 * A body with none of the known envelopes — or no JSON body at all —
 * is a parse failure, never a clean empty: it throws
 * "Unexpected response shape" so the scheduler shows an error instead
 * of reading "no scheduled jobs". */
internal fun parseJobList(body: String): JobList {
    val out = mutableListOf<CronJob>()
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException("Unexpected response shape")
    val arr = root.optJSONArray("jobs")
        ?: root.optJSONArray("data")
        ?: root.optJSONArray("items")
        ?: throw RuntimeException("Unexpected response shape")
    var dropped = 0
    for (i in 0 until arr.length()) {
        val job = arr.optJSONObject(i)?.let { parseJob(it) }
        if (job != null) out.add(job) else dropped++
    }
    if (dropped > 0) {
        // Same diagnostics deal as the task page: background readers
        // take `.jobs` and never see the count (see TasksApi).
        runCatching {
            Log.w("JobsApi", "dropped $dropped unparseable job row(s) in list response")
        }
    }
    return JobList(out, dropped)
}

/**
 * Scheduler client over the gateway's native Jobs API (same bearer auth as
 * chat, served through the proxy's /p/ route — no server changes needed).
 * Per-row parsing stays lenient across gateway versions (missing fields
 * show as blank rather than failing the whole list), but an
 * unrecognised list envelope is a failure, never an empty list (#430).
 */
class JobsApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()
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
                "POST" -> builder.post(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
                "PATCH" -> builder.patch(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
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

    /** Lists jobs on the scheduler profile (gateway home). */
    suspend fun list(path: String): Result<JobList> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/jobs").map { parseJobList(it) }
        }

    /** Creates a job: prompt + schedule expression + delivery target. */
    suspend fun create(
        path: String,
        name: String,
        prompt: String,
        schedule: String,
        delivery: String,
    ): Result<Unit> = withContext(Dispatchers.IO) {
        val body = JSONObject()
            .put("prompt", prompt)
            .put("schedule", schedule)
            .put("deliver", delivery.ifEmpty { "local" })
        if (name.isNotBlank()) body.put("name", name)
        call("POST", "$path/api/jobs", body).map { }
    }

    /** Updates a job's prompt/schedule/name/delivery. */
    suspend fun update(
        path: String,
        id: String,
        name: String,
        prompt: String,
        schedule: String,
        delivery: String,
    ): Result<Unit> = withContext(Dispatchers.IO) {
        val body = JSONObject()
        if (name.isNotBlank()) body.put("name", name)
        if (prompt.isNotBlank()) body.put("prompt", prompt)
        if (schedule.isNotBlank()) body.put("schedule", schedule)
        if (delivery.isNotBlank()) body.put("deliver", delivery)
        call("PATCH", "$path/api/jobs/$id", body).map { }
    }

    suspend fun pause(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            // Native pause endpoint, with PATCH fallback for older gateways.
            val direct = call("POST", "$path/api/jobs/$id/pause")
            if (direct.isSuccess) return@withContext direct.map { }
            call("PATCH", "$path/api/jobs/$id", JSONObject().put("paused", true)).map { }
        }

    suspend fun resume(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            val direct = call("POST", "$path/api/jobs/$id/resume")
            if (direct.isSuccess) return@withContext direct.map { }
            call("PATCH", "$path/api/jobs/$id", JSONObject().put("paused", false)).map { }
        }

    suspend fun runNow(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/jobs/$id/run").map { }
        }

    suspend fun delete(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("DELETE", "$path/api/jobs/$id").map { }
        }
}
