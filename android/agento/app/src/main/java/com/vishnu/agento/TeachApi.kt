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

/** One user-taught skill: draft → review/edit → test → activate (#314).
 * Only active skills reach the model; drafts and disabled rows are inert. */
data class TaughtSkill(
    val id: String,
    val profile: String = "",
    val name: String = "",
    val description: String = "",
    val body: String = "",
    val status: String = "",
    val source: String = "",
    val testedAt: Long = 0L,
    val testText: String = "",
    val createdAt: Long = 0L,
    val updatedAt: Long = 0L,
) {
    fun isDraft(): Boolean = status == "draft"
    fun isActive(): Boolean = status == "active"
    fun isDisabled(): Boolean = status == "disabled"
    fun isTested(): Boolean = testedAt > 0
}

/** One taught skill parsed leniently: missing id means the row is dropped.
 * Strings-only via optStr (MemoryApi): a stray 42 reads blank, and a blank
 * id drops the row. */
internal fun parseTaughtSkill(o: JSONObject): TaughtSkill? {
    val id = optStr(o, "id")
    if (id.isEmpty()) return null
    return TaughtSkill(
        id = id,
        profile = optStr(o, "profile"),
        name = optStr(o, "name"),
        description = optStr(o, "description"),
        body = optStr(o, "body"),
        status = optStr(o, "status"),
        source = optStr(o, "source"),
        // Timestamps are numeric-only by server contract: a string stamp
        // reads 0 (untested/unknown) rather than coercing.
        testedAt = o.optLong("tested_at", 0L),
        testText = optStr(o, "test_text"),
        createdAt = o.optLong("created_at", 0L),
        updatedAt = o.optLong("updated_at", 0L),
    )
}

/**
 * Client over the gateway's taught-skills API (same bearer auth as chat,
 * served through the proxy's /p/ route — always call with the profile path).
 * Parsing is lenient; undecodable rows are logged and skipped rather than
 * failing the list. Errors name the HTTP code, never the body.
 */
class TeachApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        // Test fires run a model call (up to the server's 120s budget), so
        // the read timeout covers that instead of the usual 30s.
        .readTimeout(150, TimeUnit.SECONDS)
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

    /** Non-200 is an error naming the code only: bodies carry skill text
     * (user content) that must not reach logcat or crash reports. */
    private fun checkOk(it: CallResult): CallResult {
        if (it.code != 200) throw RuntimeException("HTTP ${it.code}")
        return it
    }

    /** A 200 whose body is not JSON is a broken payload, not an empty answer. */
    private fun parseObject(body: String): JSONObject =
        runCatching { JSONObject(body) }.getOrElse {
            throw RuntimeException("unparseable response body")
        }

    /** Lists this profile's taught skills, newest first. */
    suspend fun listSkills(path: String): Result<List<TaughtSkill>> =
        withContext(Dispatchers.IO) {
            call("GET", "$path/api/skills").mapCatching {
                // Null (unparseable) fails instead of wiping the screen.
                parseSkillList(checkOk(it).body)
                    ?: throw RuntimeException("unparseable response body")
            }
        }

    /** Creates a draft: always draft, never active on arrival. Returns
     * the skill plus whether the body clipped at the server cap. */
    suspend fun createDraft(
        path: String,
        name: String,
        description: String,
        body: String,
        source: String = "",
    ): Result<Pair<TaughtSkill, Boolean>> = withContext(Dispatchers.IO) {
        val req = JSONObject()
            .put("name", name)
            .put("description", description)
            .put("body", body)
        if (source.isNotBlank()) req.put("source", source)
        call("POST", "$path/api/skills", req).mapCatching {
            val root = parseObject(checkOk(it).body)
            val skill = parseTaughtSkill(root.optJSONObject("skill") ?: JSONObject())
                ?: throw RuntimeException("response had no skill")
            skill to root.optBoolean("truncated", false)
        }
    }

    /** Edits name/description/body. Null fields are left alone. */
    suspend fun updateSkill(
        path: String,
        id: String,
        name: String? = null,
        description: String? = null,
        body: String? = null,
    ): Result<TaughtSkill> = withContext(Dispatchers.IO) {
        val req = JSONObject()
        if (name != null) req.put("name", name)
        if (description != null) req.put("description", description)
        if (body != null) req.put("body", body)
        call("PUT", "$path/api/skills/${ApprovalsApi.encode(id)}", req).mapCatching {
            parseTaughtSkill(parseObject(checkOk(it).body).optJSONObject("skill") ?: JSONObject())
                ?: throw RuntimeException("response had no skill")
        }
    }

    /** Deletes one skill. Its file drops on the next reconcile. */
    suspend fun deleteSkill(path: String, id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("DELETE", "$path/api/skills/${ApprovalsApi.encode(id)}").mapCatching {
                checkOk(it)
            }.map { }
        }

    /** Runs the procedure once through the model and records the
     * transcript. Returns the output text. */
    suspend fun testSkill(
        path: String,
        id: String,
        input: String = "",
    ): Result<String> = withContext(Dispatchers.IO) {
        val req = JSONObject()
        if (input.isNotBlank()) req.put("input", input)
        call("POST", "$path/api/skills/${ApprovalsApi.encode(id)}/test", req).mapCatching {
            // Missing output is unparseable, not empty: a 200 without the
            // transcript must fail rather than show a blank test. has()
            // rather than a sentinel default: a real output could spell
            // anything.
            val root = parseObject(checkOk(it).body)
            if (!root.has("output")) throw RuntimeException("response had no output")
            root.optString("output", "")
        }
    }

    /** Moves a tested draft to active (or re-activates a disabled row).
     * Untested drafts fail with 400: review/test before run. */
    suspend fun activateSkill(path: String, id: String): Result<TaughtSkill> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/skills/${ApprovalsApi.encode(id)}/activate").mapCatching {
                parseTaughtSkill(parseObject(checkOk(it).body).optJSONObject("skill") ?: JSONObject())
                    ?: throw RuntimeException("response had no skill")
            }
        }

    /** Disables a skill: its file drops, the model stops seeing it. */
    suspend fun disableSkill(path: String, id: String): Result<TaughtSkill> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/skills/${ApprovalsApi.encode(id)}/disable").mapCatching {
                parseTaughtSkill(parseObject(checkOk(it).body).optJSONObject("skill") ?: JSONObject())
                    ?: throw RuntimeException("response had no skill")
            }
        }

    /** Promotes a tested, active skill to a scheduled routine. Returns the
     * created job's id. The job carries a copy of the reviewed text. */
    suspend fun promoteSkill(
        path: String,
        id: String,
        schedule: String,
        name: String = "",
        delivery: String = "",
    ): Result<String> = withContext(Dispatchers.IO) {
        val req = JSONObject().put("schedule", schedule)
        if (name.isNotBlank()) req.put("name", name)
        if (delivery.isNotBlank()) req.put("delivery", delivery)
        call("POST", "$path/api/skills/${ApprovalsApi.encode(id)}/promote", req).mapCatching {
            parseObject(checkOk(it).body).optJSONObject("job")?.optString("id", "").orEmpty()
                .ifBlank { throw RuntimeException("response had no job") }
        }
    }
}

/** Parses one skills list leniently, shared by the client and its tests.
 * Null when the whole body is unparseable or the "skills" key is missing:
 * the caller fails instead of wiping the screen, so a transient
 * 200-with-garbage keeps old rows behind an error. Present rows with
 * missing fields read blank. */
internal fun parseSkillList(body: String): List<TaughtSkill>? {
    val root = runCatching { JSONObject(body) }.getOrNull()
    if (root == null) {
        // Length only: the body is user content (PII), same rationale as
        // the code-only error messages.
        runCatching { Log.w("TeachApi", "unparseable skills body (${body.length} chars)") }
        return null
    }
    val out = mutableListOf<TaughtSkill>()
    var dropped = 0
    // A missing key is a contract break, not an empty list: failing keeps
    // old rows behind an error instead of wiping the section.
    val arr = root.optJSONArray("skills") ?: return null
    for (i in 0 until arr.length()) {
        val obj = arr.optJSONObject(i)
        if (obj == null) {
            dropped++
            continue
        }
        val row = parseTaughtSkill(obj)
        if (row == null) {
            dropped++
            continue
        }
        out.add(row)
    }
    if (dropped > 0) {
        runCatching { Log.w("TeachApi", "dropped $dropped undecodable skill row(s)") }
    }
    return out
}
