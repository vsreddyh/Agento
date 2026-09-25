package com.vishnu.agento

import android.content.Context
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.Locale
import java.util.UUID

/**
 * Built-in Android text-to-speech for the three chats (no permission,
 * no extra dependency — the framework binds the system engine, so no
 * manifest entry is needed either).
 *
 * One instance is hoisted at the activity level and shared by
 * God/Story/Portfolio; each tab's [ChatScreen] stops it when the tab
 * is left or a new message is sent.
 */
class ChatTts(context: Context) {

    private val appCtx = context.applicationContext

    /** Key of the message currently being spoken, null when idle. */
    private val _speakingKey = MutableStateFlow<String?>(null)
    val speakingKey: StateFlow<String?> = _speakingKey

    private var engine: TextToSpeech? = null
    private var ready = false
    private var failed = false
    private var pending: Pair<String, String>? = null

    init {
        engine = TextToSpeech(appCtx) { status ->
            ready = status == TextToSpeech.SUCCESS
            failed = !ready
            if (ready) {
                pickLanguage()
                pending?.let { (key, text) ->
                    pending = null
                    speakInternal(key, text)
                }
            } else {
                pending = null
            }
        }
        engine?.setOnUtteranceProgressListener(object : UtteranceProgressListener() {
            override fun onStart(utteranceId: String) = Unit
            override fun onDone(utteranceId: String) {
                if (_speakingKey.value == utteranceId) _speakingKey.value = null
            }
            @Deprecated("Deprecated in Java")
            override fun onError(utteranceId: String) {
                if (_speakingKey.value == utteranceId) _speakingKey.value = null
            }
            override fun onError(utteranceId: String, errorCode: Int) {
                if (_speakingKey.value == utteranceId) _speakingKey.value = null
            }
        })
    }

    /** Device default first, US English fallback; keeps whatever works. */
    private fun pickLanguage() {
        val tts = engine ?: return
        val candidates = listOf(Locale.getDefault(), Locale.US)
        for (loc in candidates) {
            runCatching {
                val r = tts.isLanguageAvailable(loc)
                if (r == TextToSpeech.LANG_AVAILABLE ||
                    r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                    r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
                ) {
                    tts.language = loc
                    return
                }
            }
        }
    }

    /**
     * Speaks [text] under [key]; tapping the same key again stops.
     * Returns false when the engine failed to init (caller shows a hint).
     */
    fun toggle(key: String, text: String): Boolean {
        if (_speakingKey.value == key) {
            stop()
            return true
        }
        stop()
        val clean = cleanForSpeech(text)
        if (clean.isEmpty()) return true
        if (!ready) {
            // Engine still initing: queue one utterance. A failed engine
            // reports false so the caller can show a hint instead.
            if (failed) return false
            pending = key to clean
            return true
        }
        speakInternal(key, clean)
        return true
    }

    private fun speakInternal(key: String, text: String) {
        val tts = engine ?: return
        _speakingKey.value = key
        // ~3500-char chunks at sentence boundaries (engine limit is ~4000).
        var first = true
        for (chunk in chunk(text)) {
            tts.speak(
                chunk,
                if (first) TextToSpeech.QUEUE_FLUSH else TextToSpeech.QUEUE_ADD,
                null,
                if (first) key else "$key#${UUID.randomUUID()}",
            )
            first = false
        }
    }

    fun stop() {
        pending = null
        runCatching { engine?.stop() }
        _speakingKey.value = null
    }

    fun shutdown() {
        stop()
        runCatching { engine?.shutdown() }
        engine = null
        ready = false
        failed = true
    }

    companion object {
        private const val MAX_CHUNK = 3500

        /** Splits on sentence ends so chunks don't cut mid-thought. */
        fun chunk(text: String): List<String> {
            if (text.length <= MAX_CHUNK) return listOf(text)
            val out = mutableListOf<String>()
            var start = 0
            while (start < text.length) {
                var end = minOf(start + MAX_CHUNK, text.length)
                if (end < text.length) {
                    val cut = text.lastIndexOfAny(
                        charArrayOf('.', '!', '?', '\n'),
                        end - 1,
                    )
                    if (cut > start) end = cut + 1
                }
                out.add(text.substring(start, end))
                start = end
            }
            return out
        }

        /** Strips Markdown formatting so it isn't read aloud literally. */
        fun cleanForSpeech(raw: String): String {
            var s = raw
            s = s.replace(Regex("(?s)```.*?```"), " code ")
            s = s.replace(Regex("`([^`]*)`"), "$1")
            s = s.replace(Regex("!\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("(?m)^\\s{0,3}#{1,6}\\s+"), "")
            s = s.replace(Regex("[*_~]{1,3}"), "")
            s = s.replace(Regex("(?m)^\\s*[-*+]\\s+"), "")
            s = s.replace(Regex("(?m)^\\s*\\d+[.)]\\s+"), "")
            s = s.replace(Regex("\\s+"), " ").trim()
            return s
        }
    }
}
