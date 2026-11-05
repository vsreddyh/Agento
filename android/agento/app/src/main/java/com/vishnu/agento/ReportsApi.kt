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

/** One ∃-Mail report summary (list rows carry no body — `get` fetches it). */
data class ReportSummary(
    val id: String,
    val title: String = "",
    val createdAt: String = "",
    val read: Boolean = false,
    val starred: Boolean = false,
    val preview: String = "",
)

/** One segment of a rendered report body: markdown text or pre-rendered math. */
data class ReportSegment(
    /** "text" (markdown, mikepenz renders) or "math" (HTML, WebView shows). */
    val kind: String,
    val text: String = "",
    val tex: String = "",
    val html: String = "",
    val display: Boolean = false,
)

/** One full ∃-Mail report with server-rendered segments. */
data class ReportDetail(
    val id: String,
    val title: String = "",
    val createdAt: String = "",
    val source: String = "",
    val read: Boolean = false,
    val starred: Boolean = false,
    val bodyMd: String = "",
    val segments: List<ReportSegment> = emptyList(),
    val mathEngine: String = "",
)

internal fun parseReportSummary(o: JSONObject): ReportSummary? {
    val id = o.optString("id", "")
        .ifEmpty { o.optString("_id", "") }
        .trim()
    if (id.isEmpty()) return null
    return ReportSummary(
        id = id,
        title = o.optString("title", "").trim(),
        createdAt = o.optString("createdAt", "").trim(),
        read = o.optBoolean("read", false),
        starred = o.optBoolean("starred", false),
        preview = o.optString("preview", "").trim(),
    )
}

internal fun parseReportSegment(o: JSONObject): ReportSegment? {
    val kind = o.optString("kind", "").trim()
    if (kind != "text" && kind != "math") return null
    return ReportSegment(
        kind = kind,
        text = o.optString("text", ""),
        tex = o.optString("tex", ""),
        html = o.optString("html", ""),
        display = o.optBoolean("display", false),
    )
}

internal fun parseReportDetail(o: JSONObject): ReportDetail? {
    val id = o.optString("id", "")
        .ifEmpty { o.optString("_id", "") }
        .trim()
    if (id.isEmpty()) return null
    val segments = mutableListOf<ReportSegment>()
    val arr = o.optJSONArray("segments")
    if (arr != null) {
        for (i in 0 until arr.length()) {
            val s = arr.optJSONObject(i) ?: continue
            parseReportSegment(s)?.let { segments.add(it) }
        }
    }
    return ReportDetail(
        id = id,
        title = o.optString("title", "").trim(),
        createdAt = o.optString("createdAt", "").trim(),
        source = o.optString("source", "").trim(),
        read = o.optBoolean("read", false),
        starred = o.optBoolean("starred", false),
        bodyMd = o.optString("body_md", ""),
        segments = segments,
        mathEngine = o.optString("math_engine", "").trim(),
    )
}

/**
 * ∃-Mail client over health-api (`/api/reports`, same bearer auth and
 * profile-path prefix as the other API clients). Parsing is lenient across
 * server versions; missing fields show as blank rather than failing the
 * whole list. No compose/reply/send paths exist — the inbox is read-only.
 */
class ReportsApi(context: Context) {

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

    private fun parseSummaries(body: String): List<ReportSummary> {
        val out = mutableListOf<ReportSummary>()
        val root = runCatching { JSONObject(body) }.getOrNull() ?: return out
        val arr = root.optJSONArray("reports") ?: return out
        for (i in 0 until arr.length()) {
            val o = arr.optJSONObject(i) ?: continue
            parseReportSummary(o)?.let { out.add(it) }
        }
        return out
    }

    /** Lists reports, newest first. Summaries carry no bodies. */
    suspend fun list(path: String): Result<List<ReportSummary>> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/reports?sort=newest").map { parseSummaries(it) }
        }

    /** Gets one report with server-rendered math segments. */
    suspend fun get(path: String, id: String): Result<ReportDetail?> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/reports/$id").map { body ->
                runCatching { parseReportDetail(JSONObject(body)) }.getOrNull()
            }
        }

    /** Marks read and/or toggles the star. Only sent keys change. */
    suspend fun patch(path: String, id: String, read: Boolean?, starred: Boolean?): Result<Unit> =
        withContext(Dispatchers.IO) {
            val body = JSONObject()
            if (read != null) body.put("read", read)
            if (starred != null) body.put("starred", starred)
            call("PATCH", "$path/api/reports/$id", body).map { }
        }

    /** Hard-deletes one report. No undo, no trash — the client confirms first. */
    suspend fun delete(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("DELETE", "$path/api/reports/$id").map { }
        }
}
