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

/** A jobs list response: the rows plus the unparseable-drop count
 * (#430: counted and surfaced, never silent). */
data class JobList(
    val jobs: List<CronJob>,
    val dropped: Int = 0,
)

/**
 * Scheduler client over the gateway's native Jobs API (same bearer auth as
 * chat, served through the proxy's /p/ route — no server changes needed).
 * Field parsing is lenient across gateway versions; missing fields show as
 * blank rather than failing the row. The envelope itself is strict (#433:
 * no `data`/`items` downgrade — a gateway answering another shape fails
 * the whole list loudly instead of rendering as "no jobs").
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

    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

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

    private fun parseJob(o: JSONObject): CronJob? {
        val id = o.optString("id", "")
            .ifEmpty { o.optString("job_id", "") }
            .trim()
        if (id.isEmpty()) return null
        val paused = when {
            o.has("paused") -> o.optBoolean("paused")
            o.has("enabled") -> !o.optBoolean("enabled", true)
            o.has("state") -> o.optString("state", "").lowercase() == "paused"
            o.has("status") -> o.optString("status", "").lowercase() == "paused"
            else -> null
        }
        return CronJob(
            id = id,
            name = o.optString("name", "").trim(),
            prompt = o.optString("prompt", "").trim(),
            schedule = o.optString("schedule", "")
                .ifEmpty { o.optString("cron", "") }.trim(),
            paused = paused,
            lastRun = o.optString("last_run", "")
                .ifEmpty { o.optString("lastRun", "") }.trim(),
            nextRun = o.optString("next_run", "")
                .ifEmpty { o.optString("nextRun", "") }.trim(),
            delivery = o.optString("delivery", "")
                .ifEmpty { o.optString("deliver", "") }.trim(),
            runs = o.optJSONArray("runs")?.let { arr ->
                buildList {
                    for (i in 0 until arr.length()) {
                        val r = arr.optJSONObject(i) ?: continue
                        add(
                            CronRun(
                                at = r.optLong("at", 0L),
                                text = r.optString("text", "").trim(),
                                missed = r.optBoolean("missed", false),
                            )
                        )
                    }
                }
            }.orEmpty(),
        )
    }

    /** Parses the canonical `jobs` envelope (#433: no `data`/`items`
     * downgrade — a gateway answering another shape fails loudly as
     * "unexpected shape" instead of rendering as "no jobs"). Id-less rows
     * are counted, never silently dropped (#430). */
    private fun parseList(body: String): Result<JobList> {
        val out = mutableListOf<CronJob>()
        val root = runCatching { JSONObject(body) }.getOrNull()
            ?: return Result.failure(IllegalStateException("unexpected jobs response shape"))
        val arr = root.optJSONArray("jobs")
            ?: return Result.failure(
                IllegalStateException("unexpected jobs response shape (gateway too old?)"))
        var dropped = 0
        for (i in 0 until arr.length()) {
            // Same counted-drop contract as the tasks envelope (#447
            // review): a non-object entry counts, it never just vanishes.
            val o = arr.optJSONObject(i)
            if (o == null) {
                dropped++
                continue
            }
            val row = parseJob(o)
            if (row == null) {
                dropped++
            } else {
                out.add(row)
            }
        }
        if (dropped > 0) {
            runCatching { Log.w("JobsApi", "dropped $dropped unparseable job row(s)") }
        }
        return Result.success(JobList(out, dropped))
    }

    /** Lists jobs on the scheduler profile (gateway home). */
    suspend fun list(path: String): Result<JobList> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/jobs").map { parseList(it).getOrThrow() }
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
            // Native pause endpoint only (#433): no PATCH downgrade for
            // older gateways — an old gateway fails loudly here instead of
            // half-pausing through a different verb.
            call("POST", "$path/api/jobs/$id/pause").map { }
        }

    suspend fun resume(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/jobs/$id/resume").map { }
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
