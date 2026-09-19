package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.TimeUnit

data class ChatMessage(
    val role: String, // "user" | "assistant"
    val content: String,
)

sealed interface ChatEvent {
    data class Delta(val text: String) : ChatEvent
    data class Done(val fullText: String) : ChatEvent
    data class Error(val message: String) : ChatEvent
}

/** Known provider slugs: offline fallback for the dynamic picker (the live
 * list comes from GET /api/model/options on the gateway). */
enum class LlmProvider(val id: String) {
    OPENCODE("opencode"),
    OPENCODE_GO("opencode-go"),
}

/** One row of the gateway picker inventory (`GET /api/model/options`). */
data class ProviderOption(
    val slug: String,
    val label: String,
    val models: List<String>,
)

/**
 * Minimal OpenAI-compatible chat client for the Hermes API server
 * (gateway :8642, via the single-URL proxy). One server URL + single app
 * password; each tab talks to its profile path (`/p/<profile>/...`) and
 * sends its own provider + model per request. Streaming via SSE.
 * Provider/model dropdown options come from `GET /api/model/options`.
 */
class ChatApi(context: Context) {

    private val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.SECONDS) // streaming: no read timeout
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()
    }

    /** Single server URL (proxy: chat + sync on one port). Prefers the
     * unified `server_base_url`; falls back to the legacy per-feature keys
     * so pre-unification configs and backups keep working. */
    fun baseUrl(): String {
        val unified = (prefs.getString("server_base_url", "") ?: "").trim().trimEnd('/')
        if (unified.isNotEmpty()) return unified
        return (prefs.getString("api_base_url", "") ?: "").trim().trimEnd('/')
    }
    /** Single app password (the sync token doubles as chat credential).
     * Prefers `app_password`; falls back to the legacy `auth_token` so users
     * who configured a sync token keep working without re-entry. The retired
     * `api_key` is deliberately NOT read (the server no longer accepts it). */
    fun password(): String {
        val v = (prefs.getString("app_password", "") ?: "").trim()
        if (v.isNotEmpty()) return v
        return (prefs.getString("auth_token", "") ?: "").trim()
    }

    /** Profile path prefix for a tab, e.g. `/p/story`. Blank = gateway root. */
    fun pathFor(tab: String): String {
        val saved = (prefs.getString("path_$tab", "") ?: "").trim().trimEnd('/')
        if (saved.isNotEmpty()) return saved
        // Tab keys don't all match profile dir names (god tab -> default profile).
        return "/p/" + defaultProfileFor(tab)
    }

    /** Provider slug for a tab; blank = omit (gateway default applies). */
    fun providerFor(tab: String): String =
        (prefs.getString("provider_$tab", "") ?: "").trim()

    /** Model for a tab; blank = omit (gateway default applies). */
    fun modelFor(tab: String): String =
        (prefs.getString("model_$tab", "") ?: "").trim()

    fun setChatConfig(
        baseUrl: String,
        password: String,
        tab: String,
        provider: String,
        model: String,
        path: String,
    ) {
        prefs.edit()
            .putString("server_base_url", baseUrl.trimEnd('/'))
            .putString("app_password", password.trim())
            .putString("provider_$tab", provider.trim())
            .putString("model_$tab", model.trim())
            .putString("path_$tab", path.trim().trimEnd('/'))
            .apply()
    }

    /** Fetches the Hermes provider-aware picker inventory that backs the
     * dynamic provider/model dropdowns (`slug` + display `name` + string
     * `models` per row). Lenient by design: rows without a slug are skipped,
     * non-string model entries are skipped, and an empty result is a failure
     * so the UI falls back to the offline provider list. */
    suspend fun fetchCatalog(path: String, refresh: Boolean = false): Result<List<ProviderOption>> =
        withContext(Dispatchers.IO) {
            val base = baseUrl()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val url = "$base$path/api/model/options" + if (refresh) "?refresh=1" else ""
            val request = Request.Builder()
                .url(url)
                .header("Authorization", "Bearer ${password()}")
                .get()
                .build()
            try {
                http.newCall(request).execute().use { response ->
                    val body = response.body?.string() ?: ""
                    if (!response.isSuccessful) {
                        return@withContext Result.failure(
                            RuntimeException("HTTP ${response.code}: ${body.take(200)}")
                        )
                    }
                    val providers = mutableListOf<ProviderOption>()
                    val arr = JSONObject(body).optJSONArray("providers")
                    if (arr != null) {
                        for (i in 0 until arr.length()) {
                            val o = arr.optJSONObject(i) ?: continue
                            val slug = o.optString("slug", "").trim()
                            if (slug.isEmpty()) continue
                            val label = o.optString("name", "").trim().ifEmpty { slug }
                            val models = mutableListOf<String>()
                            val marr = o.optJSONArray("models")
                            if (marr != null) {
                                for (j in 0 until marr.length()) {
                                    val m = marr.opt(j)
                                    if (m is String && m.isNotBlank()) models.add(m.trim())
                                }
                            }
                            providers.add(ProviderOption(slug, label, models))
                        }
                    }
                    if (providers.isEmpty()) {
                        return@withContext Result.failure(RuntimeException("No providers in catalog"))
                    }
                    Result.success(providers)
                }
            } catch (e: Exception) {
                Result.failure(e)
            }
        }

    /** Streams reply deltas for [messages]; emits Done(fullText) at `[DONE]`. */
    fun streamChat(
        path: String,
        provider: String,
        model: String,
        messages: List<ChatMessage>,
    ): Flow<ChatEvent> = callbackFlow {
        val base = baseUrl()
        if (base.isEmpty()) {
            trySend(ChatEvent.Error("Server URL not configured — see Settings"))
            close()
            return@callbackFlow
        }
        val payload = JSONObject()
        // An explicit provider is always honored server-side; blanks fall
        // back to the gateway default for both provider and model.
        if (provider.isNotEmpty()) payload.put("provider", provider)
        if (model.isNotEmpty()) payload.put("model", model)
        val arr = JSONArray()
        for (m in messages) {
            arr.put(JSONObject().put("role", m.role).put("content", m.content))
        }
        payload.put("messages", arr)
        payload.put("stream", true)
        val body = payload.toString().toRequestBody(JSON)
        val request = Request.Builder()
            .url("$base$path/v1/chat/completions")
            .header("Authorization", "Bearer ${password()}")
            .header("Content-Type", "application/json")
            .header("Accept", "text/event-stream")
            .post(body)
            .build()
        val call = http.newCall(request)
        val job = launch(Dispatchers.IO) {
            try {
                call.execute().use { response ->
                    if (!response.isSuccessful) {
                        val err = response.body?.string()?.take(300) ?: ""
                        trySend(ChatEvent.Error("HTTP ${response.code}: $err"))
                        return@launch
                    }
                    val source = response.body?.source()
                    if (source == null) {
                        trySend(ChatEvent.Error("empty response body"))
                        return@launch
                    }
                    val full = StringBuilder()
                    // OkHttp in this project has no sse module; parse SSE lines manually.
                    while (!source.exhausted()) {
                        val line = source.readUtf8Line() ?: break
                        if (!line.startsWith("data:")) continue
                        val data = line.removePrefix("data:").trim()
                        if (data.isEmpty()) continue
                        if (data == "[DONE]") break
                        val delta = runCatching {
                            val choices = JSONObject(data).optJSONArray("choices") ?: return@runCatching ""
                            val choice = choices.optJSONObject(0) ?: return@runCatching ""
                            // chat completions: choices[0].delta.content;
                            // responses API: choices[0].message.content (fallback)
                            choice.optJSONObject("delta")?.optString("content")
                                ?: choice.optJSONObject("message")?.optString("content")
                                ?: ""
                        }.getOrDefault("")
                        if (delta.isNotEmpty()) {
                            full.append(delta)
                            trySend(ChatEvent.Delta(delta))
                        }
                    }
                    trySend(ChatEvent.Done(full.toString()))
                }
            } catch (e: Exception) {
                trySend(ChatEvent.Error(e.message ?: e.javaClass.simpleName))
            } finally {
                close()
            }
        }
        awaitClose {
            job.cancel()
            call.cancel()
        }
    }
}

/** Profile dir name backing an app tab (tab keys differ from profile names). */
fun defaultProfileFor(tab: String): String = when (tab) {
    "god" -> "default"
    else -> tab
}
