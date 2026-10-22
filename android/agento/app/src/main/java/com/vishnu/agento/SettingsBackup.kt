package com.vishnu.agento

import android.content.Context
import androidx.core.content.edit
import kotlinx.coroutines.CancellationException
import org.json.JSONObject

/** Hard cap on the backup peak, bytes: past ~25 MB of heap peak the
 * materialized string (~2x bytes as UTF-16 plus the JSONObject) reliably OOMs
 * low-end 1–2 GB-RAM devices mid-export — a crash, not a slow restore. The
 * meter is DISK bytes but the cap is HEAP, hence the ×2 + slack compare;
 * shared by the export boundary and the Settings pre-meter.
 *
 * UNITS, in one place (#339): the cap is 25 MB of HEAP. The user-visible
 * number is DISK bytes — the export refuses past ~12 MB on disk (the ×2
 * UTF-16 compare plus the floor slack), and the "this may take a moment"
 * warning starts at ~5 MB on disk. The three numbers describe ONE gate;
 * this line is where the conversion lives. */
internal const val BACKUP_HARD_CAP_BYTES: Long = 25_000_000
/** Headroom added to the floor meter before the hard-cap compare: prefs +
 * JSON framing + JSONObject wrapper. */
internal const val BACKUP_FLOOR_SLACK_BYTES: Long = 1_000_000

/**
 * Issue #16: settings survive upgrade, reinstall, and reinstall-after-gap.
 *
 * - Upgrade + reinstall are covered by Auto Backup (see backup_rules.xml /
 *   data_extraction_rules.xml, which include this prefs file).
 * - Reinstall-after-a-gap (cloud backup expired/evicted) is covered by manual
 *   JSON export/import below — a user-held file that never expires.
 *
 * Only the allowlisted keys are exported/imported, so a backup can never
 * clobber unrelated state (WorkManager DB, installer cache, …).
 */
object SettingsBackup {

    /** Named over-cap remedy: one spelling for the pre-meter, the in-boundary
     * wall, and the import gate — the next reword fixes every path. */
    internal fun backupTooLargeMessage(onDisk: Long): String =
        "Backup too large to export safely (${formatBytes(onDisk)} on disk). " +
            "Delete old photo threads in the thread switcher, then try again."

    // NOTE: stays 1 — import skips unknown keys and never reads the
    // version, so removing the retired story/resumes keys (#289) neither
    // breaks old-backup import (their keys are skipped) nor requires a bump.
    // Old readers ignore the additive "files" section for the same reason.
    const val VERSION = 1

    // Single chat tab since #289; retired story/resumes keys stay out of
    // the allowlists, so their values are neither exported nor imported —
    // orphaned, like the thread files below.
    private val TABS = listOf("chat")

    /** App files mirrored into backups (chat threads, tasks). */
    private fun backupFiles(): List<String> = buildList {
        for (t in TABS) add("chat_threads_$t.json")
        add("tasks.json")
    }

    /** Every string pref the app reads; import ignores anything else. */
    val STRING_KEYS: List<String> = buildList {
        // Unified server URL (proxy: chat + sync) + single app password;
        // legacy keys stay listed so old backups still restore and fall back
        // (retired api_key dropped; path_* dropped with the path field).
        add("server_base_url")
        add("app_password")
        add("api_base_url")
        for (t in TABS) {
            add("provider_$t")
            add("model_$t")
            // Current effort pick per tab; per-model memory
            // (`effort_<tab>_<model>`) is dynamic — see effortKeys() below.
            add("effort_$t")
        }
        add("server_url")
        add("auth_token")
        add("theme_mode")
        add("last_sync_at")
        // Behaviour (#355): stored TTS voice name, "" for engine default.
        add(BehaviourSettings.KEY_TTS_VOICE)
    }

    /** Boolean prefs the app reads. */
    val BOOLEAN_KEYS: List<String> = listOf(
        "first_sync_done",
        "notify_reply_done",
        // Reminders on/off — the one task_reminder_* key that is a user
        // setting rather than device-local alarm bookkeeping (#169). The
        // armed set and the budget snapshot are excluded on purpose; see the
        // note under [isWidgetDisplayKey].
        "task_reminders_enabled",
        // Auto-read (#355): a user setting like the reminders switch above,
        // previously omitted by oversight — without it an export silently
        // lost the pick while reminders survived. Additive like everything
        // else: old backups (without it) restore to off, old readers skip it.
        BehaviourSettings.KEY_TTS_AUTO,
        // Behaviour (#355): per-kind notification gates plus the health-sync
        // pause/Wi-Fi-only switches. All default to today's behaviour, so
        // old backups (without them) restore identically.
        BehaviourSettings.KEY_NOTIFY_FAILED,
        BehaviourSettings.KEY_NOTIFY_INSTRUCT,
        BehaviourSettings.KEY_NOTIFY_CRON,
        BehaviourSettings.KEY_SYNC_WIFI_ONLY,
        BehaviourSettings.KEY_SYNC_PAUSED,
    )

    /**
     * Cumulative real token totals per assistant (see UsageStore). Longs are
     * exported/imported like the string/boolean allowlists above — additive
     * and optional, so old backups (without these keys) still restore and
     * old readers ignore the extra numbers.
     *
     * Behaviour (#355) cadence/lead/TTS knobs ride the same list: they are
     * longs for exactly this reason, and the export's non-negative clamp
     * matches their coerceIn floors.
     */
    val LONG_KEYS: List<String> = UsageStore.LONG_KEYS + listOf(
        BehaviourSettings.KEY_SYNC_INTERVAL_HOURS,
        BehaviourSettings.KEY_TASK_SYNC_INTERVAL_MINUTES,
        BehaviourSettings.KEY_REMINDER_LEAD_MINUTES,
        BehaviourSettings.KEY_TTS_RATE_PCT,
        BehaviourSettings.KEY_TTS_PITCH_PCT,
    )

    /**
     * True for per-model effort memory keys (`effort_<tab>_<model>` with tab
     * in [TABS]). Shaped tight so a hand-edited backup can't pollute prefs
     * with arbitrary `effort_*` keys.
     */
    fun isEffortMemoryKey(k: String): Boolean {
        if (!k.startsWith("effort_")) return false
        for (t in TABS) {
            if (k.startsWith("effort_${t}_") && k.length > "effort_${t}_".length) return true
        }
        return false
    }

    /** Dynamic per-model effort keys, enumerated from ONE prefs snapshot: the
     * caller passes the same `all` map it type-checks against, so a write
     * landing mid-export cannot split check-vs-read across two reads. */
    private fun effortKeys(all: Map<String, *>): List<String> =
        all.keys.filter { k ->
            isEffortMemoryKey(k) && all[k] is String
        }.sorted()

    /**
     * Per-widget display settings, keyed by the widget id the user configured:
     * `task_widget_{view,density,due,scroll}_<appWidgetId>`, written by
     * `TaskWidgetConfigActivity.onSave` and read by the widget rows (#169).
     *
     * Same shape as the effort-memory keys — enumerated live because the
     * suffix is a runtime id, and shaped tight so a hand-edited backup cannot
     * inject arbitrary `task_widget_*` prefs: four prefixes, each with its own
     * value type, and a numeric suffix (widget ids are ints — anything else
     * matches nothing). Values are mixed: `view` and `density` are enum names,
     * `due` and `scroll` are booleans, so export writes, and import accepts,
     * each by its own type. A mistyped value in a hand-edited backup is
     * skipped rather than coerced, exactly like every other key.
     *
     * Deliberately NOT backed up, and the reason matters: the two other
     * `task_reminder_*` prefs are device-local alarm bookkeeping, not settings.
     * `task_reminder_armed` is the set of currently-scheduled alarms — restoring
     * it would make a new device believe alarms it never scheduled are armed,
     * and the refresh diff would skip arming them. `task_reminder_budget` is the
     * last refresh's six counts, recomputed on every refresh. The one reminder
     * key that is a setting is `task_reminders_enabled` in [BOOLEAN_KEYS].
     */
    private val STRING_WIDGET_PREFIXES = listOf(
        "task_widget_view_",
        "task_widget_density_",
    )
    private val BOOLEAN_WIDGET_PREFIXES = listOf(
        "task_widget_due_",
        "task_widget_scroll_",
    )

    /** True for a per-widget display key `task_widget_{view,density,due,scroll}_<id>`. */
    fun isWidgetDisplayKey(k: String): Boolean =
        isStringWidgetKey(k) || isBooleanWidgetKey(k)

    /** True for a `task_widget_{view,density}_<numeric id>` key (string-valued). */
    fun isStringWidgetKey(k: String): Boolean =
        STRING_WIDGET_PREFIXES.any { hasNumericSuffix(k, it) }

    /** True for a `task_widget_{due,scroll}_<numeric id>` key (boolean-valued). */
    fun isBooleanWidgetKey(k: String): Boolean =
        BOOLEAN_WIDGET_PREFIXES.any { hasNumericSuffix(k, it) }

    private fun hasNumericSuffix(k: String, prefix: String): Boolean {
        if (!k.startsWith(prefix) || k.length == prefix.length) return false
        return k.substring(prefix.length).all { it.isDigit() }
    }

    /** Metered footprint of everything [export] would read: file lengths only,
     * no reads — so the caller can warn BEFORE the materialization peak. Raw
     * disk bytes (a FLOOR): the export string runs ~2x bytes as UTF-16, so
     * callers judging the heap peak must scale ×2 (+ slack). Deliberately
     * UNGATED: File.length() is a metadata stat, so any thread may call it. */
    internal fun backupFilesBytes(dir: java.io.File): Long =
        backupFiles().sumOf { name ->
            // isFile, not exists (#339): a directory at a thread-file path
            // would report metadata bytes and under-read the floor.
            java.io.File(dir, name).takeIf { it.isFile }?.length() ?: 0L
        }
    /** One export's result (#339): the payload plus the files that could not
     * be read. A thread file that fails to read is simply MISSING from the
     * backup, and reporting that beats a "complete" backup that quietly
     * lost a conversation. */
    data class Export(val json: JSONObject, val unreadable: List<String>)

    /** One import's result (#339): what was applied plus the files that were
     * skipped (invalid shape, unreadable, or unwritable). A skipped thread
     * file is NOT restored, so the count has to reach the user. */
    data class Outcome(val applied: Int, val skipped: List<String>)

    /** Full-settings export (prefs + thread files) as one JSONObject; absent
     * keys are omitted (not nulled). IO-REQUIRED (enforced via
     * requireIoThread): a photo-heavy export holds ~2x peak on the caller's
     * thread — the same Main-never rule as ChatThreads.load(). */
    @androidx.annotation.WorkerThread
    fun export(context: Context): Export {
        requireIoThread("SettingsBackup.export")
        // The cap lives HERE, not only in the caller: the wall belongs on the
        // boundary that builds the peak, so a caller that skips the Settings
        // pre-meter still fails closed. Both use the same constant + wording.
        // DISK BYTES (File.length of the thread files) x2 + floor slack: the
        // cap is a HEAP peak and the export string runs ~2x its size as
        // UTF-16. This is the AUTHORITATIVE meter — the UI's pre-meter and
        // its post-build string meter are advisory estimates that can
        // disagree with it (a concurrent save, or UTF-16 vs disk bytes).
        val onDisk = backupFilesBytes(context.filesDir)
        if (onDisk * 2 + BACKUP_FLOOR_SLACK_BYTES > BACKUP_HARD_CAP_BYTES) {
            throw IllegalStateException(backupTooLargeMessage(onDisk))
        }
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        val values = JSONObject()
        // Single snapshot, type-guarded reads: contains()+get throws
        // ClassCastException on a mistyped value and fails the WHOLE export.
        // Skip, never coerce — import guards the same way.
        val all = prefs.all
        for (k in STRING_KEYS) {
            (all[k] as? String)?.let { values.put(k, it) }
        }
        for (k in effortKeys(all)) {
            (all[k] as? String)?.let { values.put(k, it) }
        }
        // Per-widget display settings: enumerated live because the key ends in
        // an appWidgetId, and written by type because the four fields are not
        // all one type. Sorted, so two exports of the same prefs are
        // byte-identical. Read from the same `all` snapshot as every other block.
        for (k in all.keys.filter { isWidgetDisplayKey(it) }.sorted()) {
            when (val v = all[k]) {
                is String -> values.put(k, v)
                is Boolean -> values.put(k, v)
            }
        }
        for (k in BOOLEAN_KEYS) {
            (all[k] as? Boolean)?.let { values.put(k, it) }
        }
        for (k in LONG_KEYS) {
            // Long-or-Int only: a hand-set Int still carries the count
            // (widened), but a fractional Double/Float is corruption —
            // truncating 1.9 to 1 would persist a wrong-but-plausible count.
            val n: Long? = when (val v = all[k]) {
                is Long -> v
                is Int -> v.toLong()
                else -> null
            }
            n?.let { values.put(k, maxOf(0L, it)) }
        }
        val files = JSONObject()
        // Unreadable files are named, not swallowed (#339): the backup
        // silently loses that thread, so the UI says which ones.
        val unreadable = mutableListOf<String>()
        for (name in backupFiles()) {
            val f = java.io.File(context.filesDir, name)
            if (f.exists()) {
                // Shipped raw BY DESIGN: the import boundary and load() both
                // canonicalize on the way in, so an export-side decode+re-encode
                // buys nothing and doubles the export peak. Pinned UTF-8: the
                // backup crosses devices, so the contract belongs in the call.
                runCatching {
                    files.put(name, f.readText(Charsets.UTF_8))
                }.onFailure { unreadable += name }
            }
        }
        // Post-read re-check (#339): the cap was metered BEFORE the reads,
        // and a concurrent save can grow a thread file in between — the
        // oversize payload would be built anyway, which is the very peak the
        // cap exists to prevent. Measured from the strings already read (no
        // second materialization): their UTF-16 size is the export's bulk.
        // The prefs block rides along (it is small, but it is part of the
        // export, so it counts).
        val readChars = files.keys().asSequence()
            .sumOf { files.optString(it, "").length.toLong() } +
            values.toString().length.toLong()
        if (readChars * 2 + BACKUP_FLOOR_SLACK_BYTES > BACKUP_HARD_CAP_BYTES) {
            throw IllegalStateException(backupTooLargeMessage(backupFilesBytes(context.filesDir)))
        }
        return Export(
            JSONObject().put("version", VERSION).put("values", values).put("files", files),
            unreadable,
        )
    }
    /** Applies a backup produced by [export]; unknown keys and mistyped values
     * are skipped. Version 1 backups (prefs only) still import. Returns the
     * number of prefs + files applied, plus the files that were skipped.
     * IO-REQUIRED like [export] (enforced):
     * file reads + canonicalize + prefs writes all ride the caller's thread. */
    @androidx.annotation.WorkerThread
    fun importFrom(context: Context, json: JSONObject): Result<Outcome> {
        requireIoThread("SettingsBackup.importFrom")
        return try {
            val values = json.optJSONObject("values")
                ?: return Result.failure(IllegalArgumentException("Not an Agento settings file (missing values)"))
            val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
            var applied = 0
            prefs.edit {
                for (k in STRING_KEYS) {
                    if (values.has(k) && values.opt(k) is String) {
                        putString(k, values.optString(k, ""))
                        applied++
                    }
                }
                for (k in BOOLEAN_KEYS) {
                    if (values.has(k) && values.opt(k) is Boolean) {
                        putBoolean(k, values.optBoolean(k))
                        applied++
                    }
                }
                // Real token totals: Long/Int only, mirroring export — opt()
                // then optLong() would coerce a fractional Double (1.9) into
                // a wrong-but-plausible 1. Negatives (hand-edited) clamp to 0.
                for (k in LONG_KEYS) {
                    val n: Long? = when (val v = values.opt(k)) {
                        is Long -> v
                        is Int -> v.toLong()
                        else -> null
                    }
                    if (n != null) {
                        putLong(k, maxOf(0L, n))
                        applied++
                    }
                }
                // Per-model effort memory: same allowlist shape as export.
                val names = values.keys().asSequence().toList()
                for (k in names) {
                    if (isEffortMemoryKey(k) && values.opt(k) is String) {
                        putString(k, values.optString(k, ""))
                        applied++
                    }
                }
                // Per-widget display settings: the export allowlist mirrored
                // per prefix — view/density take strings only, due/scroll take
                // booleans only. A cross-typed value in a hand-edited backup is
                // skipped: the widget reads those keys with getBoolean.
                for (k in names) {
                    if (isStringWidgetKey(k) && values.opt(k) is String) {
                        putString(k, values.optString(k, ""))
                        applied++
                    } else if (isBooleanWidgetKey(k) && values.opt(k) is Boolean) {
                        putBoolean(k, values.optBoolean(k))
                        applied++
                    }
                }
            }
            val files = json.optJSONObject("files")
            // Skipped files are named (#339): a skipped thread file is NOT
            // restored, and "Restored N setting(s)" over a dropped
            // conversation reads as a clean success.
            val skipped = mutableListOf<String>()
            if (files != null) {
                // Size gate, the export's twin (#339): a hand-crafted
                // backup can carry a thread file far past the cap, and
                // canonicalizing it would parse a 100 MB string on the very
                // device the export cap protects. Measured from the strings
                // already in memory (no second copy).
                val incomingChars = files.keys().asSequence()
                    .sumOf { files.optString(it, "").length.toLong() }
                if (incomingChars * 2 + BACKUP_FLOOR_SLACK_BYTES > BACKUP_HARD_CAP_BYTES) {
                    return Result.failure(
                        // incomingChars is the char count, the same unit the
                        // export twin passes (the message renders disk-like
                        // sizes; doubling it here would overstate it).
                        IllegalStateException(backupTooLargeMessage(incomingChars)),
                    )
                }
                // Unknown keys (a retired tab, a renamed file) are named
                // too (#339): they are not restored, and "Restored N" must
                // not hide what was left behind.
                val known = backupFiles()
                for (name in files.keys()) {
                    if (name !in known) skipped += name
                }
                for (name in known) {
                    // Absent entry, not a broken one (#339): a backup
                    // exported before the file existed simply has no entry
                    // for it — that is not a skip to report.
                    if (!files.has(name)) continue
                    val text = files.optString(name, "")
                    if (text.isEmpty()) {
                        // An empty entry is not restored, so it counts as
                        // skipped (#339) — a silent drop reads as a clean
                        // restore.
                        skipped += name
                        continue
                    }
                    if (name.startsWith("chat_threads_")) {
                        // Single parse, not probe-then-parse: the old org.json
                        // probe decoded the multi-MB string just to check shape,
                        // then canonicalizeFile decoded it AGAIN. Its null
                        // return IS the validity check (same skip contract).
                        // Valid-JSON-but-wrong-shape SKIPS: the old raw
                        // fallback wrote bytes load() cannot decode, so the
                        // next load returned emptyList() — wiping the LIVE
                        // threads the import just overwrote.
                        // Oversize persists until send, BY DESIGN (#339):
                        // canonicalize does not enforce the 1.5M staging
                        // cap, so an imported thread may hold a payload the
                        // current build would refuse to stage. The cost is
                        // a cold start that materializes it (the ADR
                        // discloses the backup growth); the alternative —
                        // trimming on import — would DELETE the user's
                        // photos at restore time, which is worse.
                        val toWrite = ChatThreads.canonicalizeFile(text)
                        if (toWrite == null) {
                            skipped += name
                            continue
                        }
                        val tab = name.removePrefix("chat_threads_").removeSuffix(".json")
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(toWrite, Charsets.UTF_8)
                            // Invalidate the save cache: the import bypasses
                            // save(), so a stale snapshot would write the
                            // pre-import bytes back over the restored file.
                            ChatThreads.invalidate(tab)
                            // Tell the live ViewModel too: it still holds the
                            // pre-import threads, and its next persist would
                            // write them back over this file.
                            ChatThreads.notifyRewritten(tab)
                            applied++
                        }.onFailure { skipped += name }
                        continue
                    }
                    // Validate JSON shape before overwriting live state
                    // (non-thread files only; thread files validate via the
                    // canonicalizeFile null above).
                    val valid = runCatching { org.json.JSONArray(text); true }
                        .recoverCatching { org.json.JSONObject(text); true }
                        .getOrDefault(false)
                    if (valid) {
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(text, Charsets.UTF_8)
                            applied++
                        }.onFailure { skipped += name }
                    } else {
                        skipped += name
                    }
                }
            }
            Result.success(Outcome(applied, skipped))
        } catch (e: CancellationException) {
            // A cancelled import is not a failure (#339): rethrow, so the
            // caller's scope sees the cancel instead of an error status.
            throw e
        } catch (e: Exception) {
            Result.failure(e)
        }
    }
}
