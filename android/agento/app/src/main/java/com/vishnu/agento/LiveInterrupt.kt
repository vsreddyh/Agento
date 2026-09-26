package com.vishnu.agento

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.media.audiofx.AcousticEchoCanceler
import androidx.core.content.ContextCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
import kotlin.math.sqrt

/**
 * Hands-free interrupt detector for live mode (#85).
 *
 * While the reply is being spoken, the system recognizer can't run (one-shot
 * UI), so this watches the mic in the background: sustained loud input means
 * the user is talking over the readout, and [onSpeech] fires (caller cuts
 * TTS — the live loop then starts the recognizer for real transcription).
 *
 * Echo defense is best-effort: VOICE_RECOGNITION source + hardware AEC when
 * present, an adaptive threshold (4x the floor measured while the readout
 * plays, so speaker bleed becomes the floor), and a 300ms sustain gate.
 * Needs RECORD_AUDIO; without it live mode still works tap-to-talk.
 */
class LiveInterrupt(private val onSpeech: () -> Unit) {

    private var job: Job? = null
    private var aec: AcousticEchoCanceler? = null

    fun hasPermission(context: Context): Boolean =
        ContextCompat.checkSelfPermission(
            context, Manifest.permission.RECORD_AUDIO,
        ) == PackageManager.PERMISSION_GRANTED

    /** Arms the detector; false when recording can't start (no mic, no permission). */
    fun start(scope: CoroutineScope): Boolean {
        if (job?.isActive == true) return true
        val rate = 16_000
        val minBuf = AudioRecord.getMinBufferSize(
            rate, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
        )
        if (minBuf <= 0) return false
        val rec = try {
            AudioRecord(
                MediaRecorder.AudioSource.VOICE_RECOGNITION,
                rate, AudioFormat.CHANNEL_IN_MONO,
                AudioFormat.ENCODING_PCM_16BIT, minBuf * 4,
            )
        } catch (e: SecurityException) {
            return false
        } catch (e: IllegalArgumentException) {
            return false
        }
        if (rec.state != AudioRecord.STATE_INITIALIZED) {
            runCatching { rec.release() }
            return false
        }
        if (AcousticEchoCanceler.isAvailable()) {
            runCatching {
                aec = AcousticEchoCanceler.create(rec.audioSessionId)?.apply { enabled = true }
            }
        }
        job = scope.launch(Dispatchers.IO) {
            try {
                rec.startRecording()
                val buf = ShortArray(minBuf / 2)
                // Calibrate: ~400ms of readout+room as the floor.
                var floorSum = 0.0
                var floorN = 0
                val calEnd = System.currentTimeMillis() + CALIBRATE_MS
                var threshold = ABSOLUTE_MIN
                var loudSince = 0L
                var badReads = 0
                while (isActive) {
                    val n = rec.read(buf, 0, buf.size)
                    if (n <= 0) {
                        // Mic error path: rec.read() blocks and ignores
                        // cancellation, so bail after a few bad reads
                        // instead of busy-spinning forever.
                        if (++badReads >= MAX_BAD_READS) return@launch
                        delay(50)
                        continue
                    }
                    badReads = 0
                    var sum = 0.0
                    for (i in 0 until n) {
                        val s = buf[i].toDouble()
                        sum += s * s
                    }
                    val rms = sqrt(sum / n)
                    val now = System.currentTimeMillis()
                    if (now < calEnd) {
                        floorSum += rms
                        floorN++
                        threshold = (floorSum / floorN * FLOOR_MULT)
                            .coerceIn(ABSOLUTE_MIN, ABSOLUTE_MAX)
                        continue
                    }
                    if (rms >= threshold) {
                        // 600ms sustained: a short readout burst or a loud
                        // calibration tail must not cut the speech that
                        // produced it (self-interrupt). Combined with the
                        // raised ABSOLUTE_MIN floor.
                        if (loudSince == 0L) loudSince = now
                        if (now - loudSince >= SUSTAIN_MS) {
                            onSpeech()
                            return@launch
                        }
                    } else {
                        loudSince = 0L
                    }
                    ensureActive()
                }
            } catch (e: SecurityException) {
                // Permission revoked mid-listen: drop out silently.
            } finally {
                runCatching { rec.stop() }
                rec.release()
                aec?.let { runCatching { it.release() } }
                aec = null
            }
        }
        return true
    }

    fun stop() {
        job?.cancel()
        job = null
        aec?.let { runCatching { it.release() } }
        aec = null
    }

    private companion object {
        const val CALIBRATE_MS = 400L
        const val SUSTAIN_MS = 600L
        const val FLOOR_MULT = 4.0
        const val ABSOLUTE_MIN = 1200.0
        const val ABSOLUTE_MAX = 3500.0
        const val MAX_BAD_READS = 20
    }
}
