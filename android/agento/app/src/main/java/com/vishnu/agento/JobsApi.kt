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
)

/**
 * Scheduler client over the gateway's native Jobs API (same bearer auth as
 * chat, served through the proxy's /p/ route — no server changes needed).
 * Parsing is lenient across gateway versions; missing fields show as blank
 * rather than failing the whole list.
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
        )
    }

    private fun parseList(body: String): List<CronJob> {
        val out = mutableListOf<CronJob>()
        val root = runCatching { JSONObject(body) }.getOrNull() ?: return out
        val arr = root.optJSONArray("jobs")
            ?: root.optJSONArray("data")
            ?: root.optJSONArray("items")
        if (arr != null) {
            for (i in 0 until arr.length()) {
                val o = arr.optJSONObject(i) ?: continue
                parseJob(o)?.let { out.add(it) }
            }
        }
        return out
    }

    /** Lists jobs on the scheduler profile (gateway home). */
    suspend fun list(path: String): Result<List<CronJob>> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/jobs").map { parseList(it) }
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
