package com.vishnu.agento

import android.content.Context
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.Locale

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
    /** Utterance ID of the last queued chunk; only its completion clears state. */
    private var lastUtteranceId = ""

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
            // Only the last chunk's completion clears state — earlier
            // chunks finishing must not flip the UI back mid-speech.
            private fun finished(utteranceId: String) {
                if (utteranceId == lastUtteranceId) {
                    lastUtteranceId = ""
                    _speakingKey.value = null
                }
            }
            override fun onDone(utteranceId: String) = finished(utteranceId)
            @Deprecated("Deprecated in Java")
            override fun onError(utteranceId: String) = finished(utteranceId)
            override fun onError(utteranceId: String, errorCode: Int) = finished(utteranceId)
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
        // Only the last chunk's ID is tracked: its completion is what
        // clears the speaking state.
        val chunks = chunk(text)
        chunks.forEachIndexed { i, part ->
            val id = if (i == 0) key else "$key#$i"
            if (i == chunks.lastIndex) lastUtteranceId = id
            val rc = tts.speak(
                part,
                if (i == 0) TextToSpeech.QUEUE_FLUSH else TextToSpeech.QUEUE_ADD,
                null,
                id,
            )
            if (rc == TextToSpeech.ERROR) {
                // A rejected chunk never fires onDone — clear state now
                // instead of sticking on the stop icon.
                stop()
                return
            }
        }
    }

    fun stop() {
        pending = null
        lastUtteranceId = ""
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

        /** Strips Markdown/HTML formatting so it isn't read aloud literally. */
        fun cleanForSpeech(raw: String): String {
            var s = raw
            s = s.replace(Regex("(?s)```.*?```"), " code ")
            s = s.replace(Regex("`([^`]*)`"), "$1")
            s = s.replace(Regex("!\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("(?m)^\\s{0,3}#{1,6}\\s+"), "")
            s = s.replace(Regex("[*_~]{1,3}"), "")
            s = s.replace(Regex("(?m)^\\s*>\\s?"), "")
            s = s.replace("|", " ")
            // Decode (not drop) entities so words survive; strip real HTML
            // tags — the leading-letter guard keeps "a < b" prose intact.
            s = s.replace("&amp;", "&")
                .replace("&lt;", "<")
                .replace("&gt;", ">")
                .replace("&quot;", "\"")
                .replace("&#39;", "'")
            s = s.replace(Regex("</?[A-Za-z][^>]*>"), " ")
            s = s.replace(Regex("(?m)^\\s*[-*+]\\s+"), "")
            s = s.replace(Regex("(?m)^\\s*\\d+[.)]\\s+"), "")
            s = s.replace(Regex("\\s+"), " ").trim()
            return s
        }
    }
}
