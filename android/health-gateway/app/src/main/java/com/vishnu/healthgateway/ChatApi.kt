package com.vishnu.healthgateway

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

/** Supported LLM providers. Names must match the gateway config
 * (`provider: opencode`, `fallback_providers: deepinfra`) and the
 * Hermes `opencode-go` provider (key: OPENCODE_GO_API_KEY in root .env). */
enum class LlmProvider(val id: String) {
    OPENCODE("opencode"),
    OPENCODE_GO("opencode-go"),
    DEEPINFRA("deepinfra"),
    ;

    companion object {
        fun fromId(id: String): LlmProvider =
            entries.firstOrNull { it.id == id } ?: OPENCODE
    }
}

/**
 * Minimal OpenAI-compatible chat client for the Hermes API server
 * (gateway :8642). One base URL + shared bearer key; each tab talks
 * to its profile path (`/p/<profile>/...`) and sends its own
 * provider + model per request. Streaming via SSE.
 */
class ChatApi(context: Context) {

    private val prefs = context.getSharedPreferences("health_gateway", Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.SECONDS) // streaming: no read timeout
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()
    }

    fun baseUrl(): String = prefs.getString("api_base_url", "") ?: ""
    fun apiKey(): String = prefs.getString("api_key", "") ?: ""

    /** Profile path prefix for a tab, e.g. `/p/story`. Blank = gateway root. */
    fun pathFor(tab: String): String {
        val saved = (prefs.getString("path_$tab", "") ?: "").trim().trimEnd('/')
        if (saved.isNotEmpty()) return saved
        // Tab keys don't all match profile dir names (god tab -> default profile).
        return "/p/" + defaultProfileFor(tab)
    }

    fun providerFor(tab: String): LlmProvider =
        LlmProvider.fromId((prefs.getString("provider_$tab", "") ?: "").trim())

    /** Model for a tab; blank = omit (gateway default applies). */
    fun modelFor(tab: String): String =
        (prefs.getString("model_$tab", "") ?: "").trim()

    fun setChatConfig(
        baseUrl: String,
        apiKey: String,
        tab: String,
        provider: LlmProvider,
        model: String,
        path: String,
    ) {
        prefs.edit()
            .putString("api_base_url", baseUrl.trimEnd('/'))
            .putString("api_key", apiKey.trim())
            .putString("provider_$tab", provider.id)
            .putString("model_$tab", model.trim())
            .putString("path_$tab", path.trim().trimEnd('/'))
            .apply()
    }

    suspend fun listModels(path: String = ""): Result<List<String>> = withContext(Dispatchers.IO) {
        val base = baseUrl()
        if (base.isEmpty()) return@withContext Result.failure(IllegalStateException("API base URL not configured"))
        val request = Request.Builder()
            .url("$base$path/v1/models")
            .header("Authorization", "Bearer ${apiKey()}")
            .get()
            .build()
        try {
            http.newCall(request).execute().use { response ->
                val body = response.body?.string() ?: ""
                if (!response.isSuccessful) return@withContext Result.failure(RuntimeException("HTTP ${response.code}: ${body.take(200)}"))
                val ids = mutableListOf<String>()
                val data = JSONObject(body).optJSONArray("data")
                if (data != null) {
                    for (i in 0 until data.length()) {
                        data.optJSONObject(i)?.optString("id")?.takeIf { it.isNotEmpty() }?.let { ids.add(it) }
                    }
                }
                Result.success(ids)
            }
        } catch (e: Exception) {
            Result.failure(e)
        }
    }

    /** Streams reply deltas for [messages]; emits Done(fullText) at `[DONE]`. */
    fun streamChat(
        path: String,
        provider: LlmProvider,
        model: String,
        messages: List<ChatMessage>,
    ): Flow<ChatEvent> = callbackFlow {
        val base = baseUrl()
        if (base.isEmpty()) {
            trySend(ChatEvent.Error("API base URL not configured — see Settings"))
            close()
            return@callbackFlow
        }
        val payload = JSONObject()
        // Explicit provider is always honored server-side; a blank model
        // falls back to the gateway default.
        payload.put("provider", provider.id)
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
            .header("Authorization", "Bearer ${apiKey()}")
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
