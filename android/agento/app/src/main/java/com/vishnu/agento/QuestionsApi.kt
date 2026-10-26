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

/**
 * Typed answer failure (#373): the status code decides whether the card
 * comes back. 404 means the turn (and its slot) is gone — restoring would
 * loop retries into a dead question — while 5xx/transport failures keep
 * the card with a retry.
 */
class QuestionHttpError(val code: Int, message: String) : RuntimeException(message)

/**
 * Answers to pending `ask_user` dialogs (#373).
 *
 * One decision, exactly once: `POST /{profile}/api/questions/{id}` with
 * `{"value":"..."}` or `{"cancelled":true}`. The gateway answers Pi under
 * the request's id and clears the slot; a repeat is a 409 (surfaced as a
 * failure so a double-tap can never look like a second answer).
 */
class QuestionsApi(context: Context) {

    companion object {
        /** Longest answer the gateway accepts (runs.MaxSteer, runes). The
         * composer caps input here so a deterministic 400 cannot loop:
         * the server stays authoritative for non-UI callers. */
        const val MAX_ANSWER_CHARS = 4000

        /** Server's message from an error body, else a status fallback. */
        fun serverMessage(body: String, code: Int): String {
            val msg = runCatching { JSONObject(body).optJSONObject("error")?.optString("message") }
                .getOrNull()?.trim().orEmpty()
            return msg.ifEmpty { "HTTP $code" }
        }

        /** URL-encodes one path segment (`+` is a space in form encoding,
         * not in a path — `%20` keeps ids exact). */
        fun encode(v: String): String =
            java.net.URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")
    }

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

    /** Posts one answer; 409 (already answered) reads as failure. */
    suspend fun answer(path: String, id: String, value: String): Result<Unit> =
        decide(path, id, JSONObject().put("value", value))

    /** Posts a cancellation; the tool fails and the turn continues. */
    suspend fun cancel(path: String, id: String): Result<Unit> =
        decide(path, id, JSONObject().put("cancelled", true))

    private suspend fun decide(path: String, id: String, body: JSONObject): Result<Unit> =
        withContext(Dispatchers.IO) {
            runCatching {
                val base = base()
                if (base.isEmpty()) throw IllegalStateException("Server URL not configured")
                if (id.isBlank()) throw IllegalArgumentException("question id is blank")
                val request = Request.Builder()
                    .url("$base$path/api/questions/${encode(id)}")
                    .header("Authorization", "Bearer ${password()}")
                    .post(body.toString().toRequestBody("application/json".toMediaType()))
                    .build()
                http.newCall(request).execute().use { response ->
                    val responseBody = response.body?.string() ?: ""
                    if (response.code == 409) {
                        throw QuestionHttpError(409, "already answered")
                    }
                    if (!response.isSuccessful) {
                        // Prefer the server's message (`{"error":{"message":...}}`)
                        // over the wire status: the card renders it verbatim,
                        // and "HTTP 400: …" names the transport, not the fix.
                        throw QuestionHttpError(response.code, serverMessage(responseBody, response.code))
                    }
                }
            }
        }
}
