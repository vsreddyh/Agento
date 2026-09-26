package com.vishnu.agento

import android.content.Context
import android.content.Intent
import android.os.Bundle
import android.speech.RecognitionListener
import android.speech.RecognizerIntent
import android.speech.SpeechRecognizer

/**
 * In-app speech recognition for live mode (#85).
 *
 * Unlike the one-shot RecognizerIntent dialog (which covers the screen and
 * swallows taps), this runs headless inside our UI, so the live overlay —
 * status, transcript, End button — stays visible and tappable throughout.
 * Must be created/used on the main thread; caller owns lifecycle via
 * [destroy]. Needs RECORD_AUDIO (live mode requests it on enable).
 */
class LiveRecognizer(context: Context) {

    interface Listener {
        fun onBegin()
        fun onPartial(text: String)
        fun onResult(text: String)
        fun onNoSpeech()
        fun onFatal(message: String)
    }

    var listener: Listener? = null

    private val appCtx = context.applicationContext
    private val recognizer: SpeechRecognizer? =
        runCatching { SpeechRecognizer.createSpeechRecognizer(appCtx) }.getOrNull()

    fun available(): Boolean =
        recognizer != null && SpeechRecognizer.isRecognitionAvailable(appCtx)

    /** Starts one listen cycle; results arrive via [listener]. */
    fun listen() {
        val rec = recognizer ?: run {
            listener?.onFatal("Voice recognition unavailable on this device.")
            return
        }
        val intent = Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH).apply {
            putExtra(
                RecognizerIntent.EXTRA_LANGUAGE_MODEL,
                RecognizerIntent.LANGUAGE_MODEL_FREE_FORM,
            )
            putExtra(RecognizerIntent.EXTRA_PARTIAL_RESULTS, true)
            putExtra(RecognizerIntent.EXTRA_MAX_RESULTS, 1)
        }
        rec.setRecognitionListener(Inner())
        runCatching { rec.startListening(intent) }
            .onFailure { listener?.onFatal("Could not start listening.") }
    }

    /** Cancels the current cycle silently (no callbacks). */
    fun cancel() {
        runCatching { recognizer?.cancel() }
    }

    fun destroy() {
        runCatching { recognizer?.cancel() }
        runCatching { recognizer?.destroy() }
    }

    private fun firstText(results: Bundle?): String =
        results?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION)
            ?.firstOrNull()?.trim().orEmpty()

    private inner class Inner : RecognitionListener {
        override fun onReadyForSpeech(params: Bundle?) {
            listener?.onBegin()
        }
        override fun onBeginningOfSpeech() = Unit
        override fun onRmsChanged(rmsdB: Float) = Unit
        override fun onBufferReceived(buffer: ByteArray?) = Unit
        override fun onEndOfSpeech() = Unit
        override fun onPartialResults(partialResults: Bundle?) {
            val text = firstText(partialResults)
            if (text.isNotEmpty()) listener?.onPartial(text)
        }
        override fun onResults(results: Bundle?) {
            val text = firstText(results)
            if (text.isEmpty()) listener?.onNoSpeech()
            else listener?.onResult(text)
        }
        override fun onEvent(eventType: Int, params: Bundle?) = Unit
        override fun onError(error: Int) {
            when (error) {
                // No usable speech: the loop counts a silent round and retries.
                SpeechRecognizer.ERROR_NO_MATCH,
                SpeechRecognizer.ERROR_SPEECH_TIMEOUT,
                SpeechRecognizer.ERROR_RECOGNIZER_BUSY,
                SpeechRecognizer.ERROR_AUDIO,
                -> listener?.onNoSpeech()
                SpeechRecognizer.ERROR_INSUFFICIENT_PERMISSIONS,
                SpeechRecognizer.ERROR_CLIENT,
                -> listener?.onFatal("Microphone unavailable — live session ended.")
                else -> listener?.onFatal("Voice recognition failed — live session ended.")
            }
        }
    }
}
