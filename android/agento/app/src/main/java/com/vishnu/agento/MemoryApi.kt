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

/** One remembered fact: agent proposals start pending, user writes are
 * confirmed, and only confirmed items steer the next turn (#315). */
data class MemoryItem(
    val id: String,
    val profile: String = "",
    val topic: String = "",
    val text: String = "",
    val source: String = "",
    val status: String = "",
    val createdAt: Long = 0L,
    val updatedAt: Long = 0L,
) {
    fun isPending(): Boolean = status == "pending"
    fun isConfirmed(): Boolean = status == "confirmed"
}

/** One memory list: rows + paused topics + the topic union for toggles.
 * Warnings is present only when the list degraded (pauses unreadable). */
data class MemoryList(
    val items: List<MemoryItem> = emptyList(),
    val paused: List<String> = emptyList(),
    val topics: List<String> = emptyList(),
    val warnings: List<String> = emptyList(),
)

/** One memory item parsed leniently: missing id means the row is dropped. */
/** Strings only, like the list collector: optString would coerce a stray
 * 42 into "42". Wrong-type fields read blank (a blank id drops the row). */
internal fun optStr(o: JSONObject, key: String): String =
    (o.opt(key) as? String)?.trim().orEmpty()

internal fun parseMemoryItem(o: JSONObject): MemoryItem? {
    val id = optStr(o, "id")
    if (id.isEmpty()) return null
    return MemoryItem(
        id = id,
        profile = optStr(o, "profile"),
        topic = optStr(o, "topic"),
        text = optStr(o, "text"),
        source = optStr(o, "source"),
        status = optStr(o, "status"),
        createdAt = o.optLong("created_at", 0L),
        updatedAt = o.optLong("updated_at", 0L),
    )
}

/** A 200 whose body is not JSON is a broken payload, not an empty answer:
 * callers render the code (no silent success, no codeless error). */
internal fun parseObject(body: String): JSONObject =
    runCatching { JSONObject(body) }.getOrElse {
        throw RuntimeException("unparseable response body")
    }

/** Null when the whole body is unparseable: the caller fails instead of
 * wiping the sheet, so a transient 200-with-garbage keeps old rows and the
 * badge behind an in-sheet error. Missing keys still read as empty. */
internal fun parseMemoryList(body: String): MemoryList? {
    val root = runCatching { JSONObject(body) }.getOrNull()
    if (root == null) {
        // Length only: the body is memory text (PII), same rationale as the
        // code-only error messages.
        runCatching { Log.w("MemoryApi", "unparseable memory body (${body.length} chars)") }
        return null
    }
    val items = mutableListOf<MemoryItem>()
    var dropped = 0
    val arr = root.optJSONArray("items")
    if (arr != null) {
        for (i in 0 until arr.length()) {
            // Non-object rows count as dropped too: both undecodable
            // shapes get the same log line, not two treatments.
            val obj = arr.optJSONObject(i)
            if (obj == null) {
                dropped++
                continue
            }
            val row = parseMemoryItem(obj)
            if (row == null) {
                dropped++
                continue
            }
            items.add(row)
        }
    }
    if (dropped > 0) {
        runCatching { Log.w("MemoryApi", "dropped $dropped undecodable memory row(s)") }
    }
    // paused encodes as [] on a fresh profile (never null per contract),
    // but a missing key still reads as empty rather than failing. Blank
    // entries (null/non-string JSON) are skipped: they would render as
    // empty toggle buttons and can never address a real topic.
    fun stringsOf(key: String): List<String> {
        val out = mutableListOf<String>()
        root.optJSONArray(key)?.let { arr ->
            for (i in 0 until arr.length()) {
                // Strings only: optString would coerce a stray 42 into
                // "42", which would render as a phantom toggle button.
                val v = (arr.opt(i) as? String)?.trim() ?: continue
                if (v.isNotBlank()) out.add(v)
            }
        }
        return out
    }
    // Duplicated entries render one toggle each; membership still
    // behaves, but the sheet should not show the same button twice.
    fun dedup(v: List<String>): List<String> = v.distinct()
    return MemoryList(items, dedup(stringsOf("paused")), dedup(stringsOf("topics")), dedup(stringsOf("warnings")))
}

/**
 * Client over the gateway's memory API (same bearer auth as chat, served
 * through the proxy's /p/ route — always call with the profile path).
 * Parsing is lenient; undecodable rows are logged and skipped rather than
 * failing the list. Errors name the HTTP code, never the body.
 */
class MemoryApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        internal fun encode(v: String): String =
            ApprovalsApi.encode(v)
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
                "PUT" -> builder.put(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
                "DELETE" -> builder.delete()
                else -> throw IllegalArgumentException("unknown method $method")
            }
            http.newCall(builder.build()).execute().use { response ->
                CallResult(response.code, response.body?.string() ?: "")
            }
        }
    }

    /** Non-200 is an error naming the code only: the snippet stays out of
     * the message because bodies carry memory text (PII) that must not reach
     * logcat or crash reports. Callers needing the body read it at debug. */
    private fun checkOk(it: CallResult): CallResult {
        if (it.code != 200) throw RuntimeException("HTTP ${it.code}")
        return it
    }

    /** Lists items with paused topics and the topic union. Null filters
     * mean no constraint; limit 0 means the server default. */
    suspend fun listMemory(
        path: String,
        status: String? = null,
        topic: String? = null,
        limit: Int = 200,
    ): Result<MemoryList> = withContext(Dispatchers.IO) {
        var url = "$path/api/memory?limit=$limit"
        if (!status.isNullOrBlank()) url += "&status=${encode(status.trim())}"
        if (!topic.isNullOrBlank()) url += "&topic=${encode(topic.trim())}"
        call("GET", url).mapCatching {
            parseMemoryList(checkOk(it).body) ?: throw RuntimeException("unparseable response body")
        }
    }

    /** Creates a user-written item: confirmed on arrival, no review loop
     * for the person whose memory it is. Returns the item + whether the
     * text clipped at the server cap. */
    suspend fun createMemory(
        path: String,
        topic: String,
        text: String,
    ): Result<Pair<MemoryItem, Boolean>> = withContext(Dispatchers.IO) {
        val body = JSONObject().put("topic", topic).put("text", text)
        call("POST", "$path/api/memory", body).mapCatching {
            val root = parseObject(checkOk(it).body)
            val item = parseMemoryItem(root.optJSONObject("item") ?: JSONObject())
                ?: throw RuntimeException("response had no item")
            item to root.optBoolean("truncated", false)
        }
    }

    /** Edits text/topic or confirms a pending item (status confirmed).
     * Null fields are left alone. */
    suspend fun updateMemory(
        path: String,
        id: String,
        text: String? = null,
        topic: String? = null,
        status: String? = null,
    ): Result<Pair<MemoryItem, Boolean>> = withContext(Dispatchers.IO) {
        val body = JSONObject()
        if (text != null) body.put("text", text)
        if (topic != null) body.put("topic", topic)
        if (status != null) body.put("status", status)
        call("PUT", "$path/api/memory/${encode(id)}", body).mapCatching {
            val root = parseObject(checkOk(it).body)
            val item = parseMemoryItem(root.optJSONObject("item") ?: JSONObject())
                ?: throw RuntimeException("response had no item")
            item to root.optBoolean("truncated", false)
        }
    }

    /** Real delete: the item is gone from the next turn. */
    suspend fun deleteMemory(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("DELETE", "$path/api/memory/${encode(id)}").mapCatching {
                checkOk(it)
            }.map { }
        }

    /** Forgets every item for the profile. Pauses survive (policy, not
     * memory). Returns how many rows went. */
    suspend fun forgetMemory(path: String): Result<Int> = withContext(Dispatchers.IO) {
        call("POST", "$path/api/memory/forget").mapCatching {
            parseObject(checkOk(it).body).optInt("deleted", 0)
        }
    }

    /** Pauses or resumes one topic. Paused items stay visible and editable;
     * they are simply not injected into turns. */
    suspend fun setTopicPaused(
        path: String,
        topic: String,
        paused: Boolean,
    ): Result<Unit> = withContext(Dispatchers.IO) {
        val body = JSONObject().put("paused", paused)
        call("PUT", "$path/api/memory/topics/${encode(topic)}", body).mapCatching {
            checkOk(it)
        }.map { }
    }

}
