package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.buffer
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
    /** Epoch millis when the message was created; 0 = unknown (legacy). */
    val ts: Long = 0L,
    /** Tool names the assistant used for this reply (persisted, [] = unknown). */
    val tools: List<String> = emptyList(),
    /** Skill names the assistant used for this reply (heuristic, see ServerApi). */
    val skills: List<String> = emptyList(),
)

sealed interface ChatEvent {
    data class Delta(val text: String) : ChatEvent
    data class Done(val fullText: String) : ChatEvent
    data class Error(val message: String) : ChatEvent
    /** Live tool-start signal from `hermes.tool.progress` SSE frames. */
    data class ToolProgress(
        val tool: String,
        val label: String = "",
        val status: String = "",
    ) : ChatEvent
}

/** Known provider slugs: offline fallback for the dynamic picker (the live
 * list comes from GET /api/model/options on the gateway). */
enum class LlmProvider(val id: String) {
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

    /** Single server URL (proxy: chat + sync on one port). One chain
     * everywhere (`server_base_url` → `server_url` → `api_base_url`) so chat,
     * sync, and Settings can never disagree pre-save; legacy keys fall back
     * for pre-unification configs and are removed on save. */
    fun baseUrl(): String {
        for (k in listOf("server_base_url", "server_url", "api_base_url")) {
            val v = (prefs.getString(k, "") ?: "").trim().trimEnd('/')
            if (v.isNotEmpty()) return v
        }
        return ""
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

    /** Profile path prefix for a tab, e.g. `/p/story`. The path field was
     * removed from Settings (defaults are always correct); this stays
     * centralized so every sender resolves identically. */
    fun pathFor(tab: String): String =
        // Tab keys don't all match profile dir names (god tab -> default profile).
        "/p/" + defaultProfileFor(tab)

    /** Provider slug for a tab; blank means not configured (legacy: omitted). */
    fun providerFor(tab: String): String =
        (prefs.getString("provider_$tab", "") ?: "").trim()

    /** Model for a tab; blank means not configured (legacy: omitted). */
    fun modelFor(tab: String): String =
        (prefs.getString("model_$tab", "") ?: "").trim()

    /**
     * Reasoning effort for a tab+model pair; blank means unset (the gateway
     * applies its own default). Per-model memory first
     * (`effort_<tab>_<model>`), falling back to the tab's last pick
     * (`effort_<tab>`). The stored value is clamped to the model's own
     * vocabulary — a pick saved for a graded model (e.g. `max`) is invalid
     * on a toggle model (`none`/`high`) and reads back as blank so callers
     * fall through to the model's default instead of showing/sending a
     * level the model can't speak.
     */
    fun effortFor(tab: String, model: String): String {
        val m = model.trim()
        var stored = ""
        if (m.isNotEmpty()) {
            stored = (prefs.getString("effort_${tab}_$m", "") ?: "").trim().lowercase()
        }
        if (stored.isEmpty()) {
            stored = (prefs.getString("effort_$tab", "") ?: "").trim().lowercase()
        }
        if (stored.isEmpty() || m.isEmpty()) return stored
        return stored.takeIf { it in EffortCatalog.optionsFor(m) } ?: ""
    }

    fun setChatConfig(
        baseUrl: String,
        password: String,
        tab: String,
        provider: String,
        model: String,
        effort: String = "",
    ) {
        val edit = prefs.edit()
            .putString("server_base_url", baseUrl.trim().trimEnd('/'))
            .putString("app_password", password.trim())
            .remove("api_base_url") // legacy: unified key is written above
            .remove("server_url") // legacy: unified key is written above
            .remove("path_$tab") // legacy: path field removed, defaults apply
            .putString("provider_$tab", provider.trim())
            .putString("model_$tab", model.trim())
        val e = effort.trim().lowercase()
        if (e.isNotEmpty()) {
            // Clamp to the model's vocabulary so a stale tab pick can never
            // be persisted under a model that can't speak it (reads back via
            // effortFor, which clamps the same way).
            val valid = if (model.trim().isEmpty()) e
                else e.takeIf { it in EffortCatalog.optionsFor(model.trim()) }
            if (valid != null) {
                edit.putString("effort_$tab", valid)
                if (model.trim().isNotEmpty()) edit.putString("effort_${tab}_${model.trim()}", valid)
            }
        }
        edit.apply()
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

    /** Result of the Settings connection test: both backends behind the
     * single server URL are probed (gateway chat + health-api sync). */
    data class ConnectionReport(
        val gatewayOk: Boolean,
        val syncOk: Boolean,
        val providers: Int = 0,
        val models: Int = 0,
        val gatewayError: String = "",
        val syncError: String = "",
    )

    /** Short-timeout client for probes (never the streaming client). */
    private fun probeClient(): OkHttpClient = OkHttpClient.Builder()
        .connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(10, TimeUnit.SECONDS)
        .writeTimeout(10, TimeUnit.SECONDS)
        .build()

    /**
     * Tests the server URL against both backends: the gateway picker
     * (authenticated, proves chat works) and /health (proves sync works —
     * served by health-api through the proxy, or the gateway itself on a
     * direct :8642 URL). Used by Settings → Test connection and the
     * offline banner. Never throws — failures land in the report.
     */
    suspend fun testConnection(path: String): Result<ConnectionReport> =
        withContext(Dispatchers.IO) {
            val base = baseUrl()
            if (base.isEmpty()) {
                return@withContext Result.failure(
                    IllegalStateException("Server URL not configured"))
            }
            val client = probeClient()
            var gatewayOk = false
            var providers = 0
            var models = 0
            var gatewayError = ""
            try {
                client.newCall(Request.Builder()
                    .url("$base$path/api/model/options")
                    .header("Authorization", "Bearer ${password()}")
                    .get()
                    .build()).execute().use { response ->
                    val body = response.body?.string() ?: ""
                    if (!response.isSuccessful) {
                        gatewayError = "HTTP ${response.code}: ${body.take(200)}"
                    } else {
                        val arr = JSONObject(body).optJSONArray("providers")
                        if (arr != null) {
                            for (i in 0 until arr.length()) {
                                val o = arr.optJSONObject(i) ?: continue
                                val slug = o.optString("slug", "").trim()
                                if (slug.isEmpty()) continue
                                providers++
                                val marr = o.optJSONArray("models")
                                if (marr != null) {
                                    for (j in 0 until marr.length()) {
                                        if (marr.opt(j) is String) models++
                                    }
                                }
                            }
                        }
                        gatewayOk = providers > 0
                        if (!gatewayOk) gatewayError = "No providers in catalog"
                    }
                }
            } catch (e: Exception) {
                gatewayError = e.message ?: e.javaClass.simpleName
            }
            var syncOk = false
            var syncError = ""
            try {
                client.newCall(Request.Builder()
                    .url("$base/health")
                    .get()
                    .build()).execute().use { response ->
                    syncOk = response.isSuccessful
                    if (!syncOk) syncError = "HTTP ${response.code}"
                }
            } catch (e: Exception) {
                syncError = e.message ?: e.javaClass.simpleName
            }
            Result.success(ConnectionReport(
                gatewayOk = gatewayOk, syncOk = syncOk,
                providers = providers, models = models,
                gatewayError = gatewayError, syncError = syncError,
            ))
    }
    /** Streams reply deltas for [messages]; emits Done(fullText) at `[DONE]`.
     * Tool-start visibility comes through as [ChatEvent.ToolProgress] parsed
     * from the gateway's `hermes.tool.progress` SSE frames. [sessionId] is
     * sent as `X-Hermes-Session-Id` (blank = omitted) so the turn can be
     * correlated with `GET api/sessions/{id}/messages` afterwards; the agent
     * loop itself is unchanged (full history is still sent per request). */
    fun streamChat(
        path: String,
        provider: String,
        model: String,
        messages: List<ChatMessage>,
        sessionId: String = "",
        effort: String = "",
    ): Flow<ChatEvent> = callbackFlow {
        val base = baseUrl()
        if (base.isEmpty()) {
            trySend(ChatEvent.Error("Server URL not configured — see Settings"))
            close()
            return@callbackFlow
        }
        val payload = JSONObject()
        // An explicit provider/model is always sent; blanks are omitted
        // (legacy configs predate required selection).
        if (provider.isNotEmpty()) payload.put("provider", provider)
        if (model.isNotEmpty()) payload.put("model", model)
        // Per-request reasoning override: the gateway translates
        // model_options.reasoning_effort into the agent's reasoning config
        // (unknown values are ignored server-side; blank = server default).
        if (effort.trim().isNotEmpty()) {
            payload.put(
                "model_options",
                JSONObject().put("reasoning_effort", effort.trim().lowercase()),
            )
        }
        val arr = JSONArray()
        for (m in messages) {
            arr.put(JSONObject().put("role", m.role).put("content", m.content))
        }
        payload.put("messages", arr)
        payload.put("stream", true)
        val body = payload.toString().toRequestBody(JSON)
        val reqBuilder = Request.Builder()
            .url("$base$path/v1/chat/completions")
            .header("Authorization", "Bearer ${password()}")
            .header("Content-Type", "application/json")
            .header("Accept", "text/event-stream")
        if (sessionId.isNotBlank()) reqBuilder.header("X-Hermes-Session-Id", sessionId.trim())
        val request = reqBuilder.post(body).build()
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
                    // `: keepalive` comments and `event:` lines carry no payload.
                    while (!source.exhausted()) {
                        val line = source.readUtf8Line() ?: break
                        if (line.startsWith(":") || line.startsWith("event:")) continue
                        if (!line.startsWith("data:")) continue
                        val data = line.removePrefix("data:").trim()
                        if (data.isEmpty()) continue
                        if (data == "[DONE]") break
                        // Tool-progress frames carry {"tool","label",...} and no
                        // choices array — surface them instead of dropping them.
                        val progress = runCatching {
                            val o = JSONObject(data)
                            if (o.optJSONArray("choices") != null) return@runCatching null
                            // A real tool name is required: label-only frames
                            // can't be attributed, so they are skipped rather
                            // than surfaced under a bogus name.
                            val tool = o.optString("tool", "").trim()
                            if (tool.isEmpty()) return@runCatching null
                            ChatEvent.ToolProgress(
                                tool = tool,
                                label = o.optString("label", "").trim(),
                                status = o.optString("status", "").trim(),
                            )
                        }.getOrNull()
                        if (progress != null) {
                            trySend(progress)
                            continue
                        }
                        // Gateway error frames ({"error": "..."}) carry no
                        // choices/tool payload — surface them instead of
                        // dropping, so the user never sees a bogus
                        // "empty reply" for a failed turn.
                        val errMsg = runCatching {
                            JSONObject(data).optString("error", "").trim()
                        }.getOrDefault("")
                        if (errMsg.isNotEmpty()) {
                            // Terminal: no Done follows (same contract as the
                            // HTTP-error path), so the consumer must not
                            // expect further frames for this turn.
                            trySend(ChatEvent.Error(errMsg))
                            return@launch
                        }
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
    }.buffer(Channel.UNLIMITED)
}

/** Profile dir name backing an app tab (tab keys differ from profile names). */
fun defaultProfileFor(tab: String): String = when (tab) {
    "god" -> "default"
    else -> tab
}

/** Display title for a chat tab key (tab keys differ from profile names). */
fun tabTitle(tab: String): String = when (tab) {
    "god" -> "God"
    "story" -> "Story"
    else -> "Resume and Portfolio"
}
