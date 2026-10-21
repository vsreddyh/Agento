package com.vishnu.agento

import android.content.Context
import androidx.core.content.edit
import org.json.JSONObject

/** Hard cap on the backup peak, bytes (#339): past ~25 MB of heap peak the
 * materialized string (~2x bytes as UTF-16 plus the JSONObject) reliably
 * OOMs low-end 1–2 GB-RAM devices mid-export — a crash, not a slow restore.
 * The meter is DISK bytes but the cap is HEAP, hence the ×2 + slack compare:
 * a backup at 12 MB on disk peaks ~25 MB and is refused. Shared by the
 * export boundary (the wall every caller hits) and the Settings pre-meter
 * (the friendly, tap-consent warning), so the two can never name different
 * limits. */
internal const val BACKUP_HARD_CAP_BYTES: Long = 25_000_000

/** Headroom added to the floor meter before the hard-cap compare (#339):
 * prefs + JSON framing + JSONObject wrapper. KBs in practice, but the gate
 * must judge the sum, not just the files. */
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

    /** Named over-cap remedy (#339): one spelling for the pre-meter, the
     * in-boundary wall, and the import gate — the next reword fixes every
     * path instead of one. */
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
    )

    /**
     * Cumulative real token totals per assistant (see UsageStore). Longs are
     * exported/imported like the string/boolean allowlists above — additive
     * and optional, so old backups (without these keys) still restore and
     * old readers ignore the extra numbers.
     */
    val LONG_KEYS: List<String> = UsageStore.LONG_KEYS

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

    /**
     * Dynamic per-model effort keys, enumerated from ONE prefs snapshot
     * (#339): the caller passes the same `all` map it type-checks against,
     * so a prefs write landing mid-export cannot list a key the snapshot
     * lacks (skipped despite existing) or split check-vs-read across two
     * reads. Only string values are exported.
     */
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

    /** Serializes all known prefs plus app files; absent keys are omitted (not nulled). */
    /**
     * Metered footprint of everything [export] would read (#339): file
     * lengths only, no reads — so the caller can warn BEFORE the
     * materialization peak instead of after it. Raw disk bytes (a FLOOR):
     * the export string runs ~2x bytes as UTF-16 plus the JSONObject copy,
     * so callers judging the heap peak must scale ×2 (+ slack) — comparing
     * raw lengths to a heap cap understates ASCII base64 ~2x (12 MB on disk
     * peaks ~25 MB in memory). Deliberately
     * UNGATED (no requireIoThread): File.length() is a metadata stat, not
     * a blocking read, so any thread may call it — the gate lives on the
     * readers (export/import/load/save), not the meter.
     */
    internal fun backupFilesBytes(dir: java.io.File): Long =
        backupFiles().sumOf { name ->
            java.io.File(dir, name).takeIf { it.exists() }?.length() ?: 0L
        }
    /**
     * Full-settings export (prefs + thread files) as one JSONObject.
     * IO-REQUIRED (see requireIoThread, enforced below — not just this
     * comment): a photo-heavy export holds ~2x peak on the caller's thread
     * (file text + JSONObject copy; the old canonical re-encode is gone —
     * import/load canonicalize on the way in), bounded by the 200-msg cap
     * — the same Main-never rule as ChatThreads.load(). Both
     * callers (Settings export + import pickers) run inside
     * withContext(Dispatchers.IO); a future Main-thread caller fails fast
     * instead of freezing the UI. Remaining hump, named not hidden (#339):
     * toString() still materializes the whole compact string (~2x bytes as
     * UTF-16) before the first chunk writes — the read/compress peaks are
     * scoped, this is the last OOM hump. Streaming via JsonWriter (or
     * excluding payloads with a re-attach note) removes it; the 5 MB
     * warning stays as the interim signal.
     */
    @androidx.annotation.WorkerThread
    fun export(context: Context): JSONObject {
        requireIoThread("SettingsBackup.export")
        // The cap lives HERE, not only in the caller (#339): every caller
        // materializes the same string peak, so the wall belongs on the
        // boundary that builds it — a share-sheet or future caller that
        // skips the Settings meter still fails closed with the named
        // remedy. Settings keeps its own pre-meter for the warning (the
        // floor is cheaper to judge before this runs); this is the
        // backstop, and both use the same constant + wording.
        val onDisk = backupFilesBytes(context.filesDir)
        if (onDisk * 2 + BACKUP_FLOOR_SLACK_BYTES > BACKUP_HARD_CAP_BYTES) {
            throw IllegalStateException(backupTooLargeMessage(onDisk))
        }
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        val values = JSONObject()
        // Single snapshot, type-guarded reads (#339): contains()+get throws
        // ClassCastException on a mistyped value (old build, hand-edit) and
        // fails the WHOLE export. Import already guards with opt-is checks;
        // export mirrors it — skip, never coerce. Snapshot once, not per
        // key, so the check and the read can't disagree mid-export either.
        val all = prefs.all
        for (k in STRING_KEYS) {
            (all[k] as? String)?.let { values.put(k, it) }
        }
        for (k in effortKeys(all)) {
            (all[k] as? String)?.let { values.put(k, it) }
        }
        // Per-widget display settings (#169): enumerated live because the key
        // ends in an appWidgetId, and written by type because the four fields
        // are not all one type. Sorted, so two exports of the same prefs are
        // byte-identical and can be diffed. Read from the same `all`
        // snapshot as every other block (#339): re-reading prefs.all per
        // key would split check-vs-read on a concurrent write.
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
            // Long-or-Int only (#339): a hand-set Int still carries the
            // count (widened), but a fractional Double/Float is corruption —
            // truncating 1.9 to 1 would persist a wrong-but-plausible
            // count. Skipped like every other mistyped value in this
            // function; import clamps its own side.
            val n: Long? = when (val v = all[k]) {
                is Long -> v
                is Int -> v.toLong()
                else -> null
            }
            n?.let { values.put(k, maxOf(0L, it)) }
        }
        val files = JSONObject()
        for (name in backupFiles()) {
            val f = java.io.File(context.filesDir, name)
            if (f.exists()) {
                // Shipped raw BY DESIGN (#339): the import boundary and load()
                // both canonicalize on the way in, so an export-side
                // decode+re-encode (2x multi-MB parses) only paid off for
                // raw-file shares that bypass load() — which self-heal on
                // first read anyway. Halves the export peak (file text +
                // JSONObject copy, no re-encode) with no contract loss; the
                // toStored precedent (raw on Main, canonicalize at the
                // boundary) already works this way.
                // Pinned UTF-8 (#339): the platform default happens to be
                // UTF-8 on Android, but the backup crosses devices (and the
                // wire path pins it explicitly) — the contract belongs in
                // the call, not in the platform's happenstance.
                runCatching {
                    files.put(name, f.readText(Charsets.UTF_8))
                }
            }
        }
        return JSONObject().put("version", VERSION).put("values", values).put("files", files)
    }

    /**
     * Applies a backup produced by [export]; unknown keys and mistyped values
     * are skipped. Version 1 backups (prefs only) still import. Returns the
     * number of prefs + files applied. IO-REQUIRED like [export] (#339,
     * enforced): file reads + canonicalize + prefs writes all ride the
     * caller's thread — the picker caller runs inside
     * withContext(Dispatchers.IO). A future Main-thread caller fails fast
     * instead of freezing the UI.
     */
    @androidx.annotation.WorkerThread
    fun importFrom(context: Context, json: JSONObject): Result<Int> {
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
                // Real token totals: JSON numbers only (optLong coerces, so the
                // raw type is checked first like the other allowlists).
                // Negatives (hand-edited files) clamp to 0 — totals never go below.
                for (k in LONG_KEYS) {
                    if (values.has(k) && values.opt(k) is Number) {
                        putLong(k, maxOf(0L, values.optLong(k, 0L)))
                        applied++
                    }
                }
                // Per-model effort memory: same allowlist shape as export
                // (isEffortMemoryKey, string values only). Unknown keys and
                // mistyped values are still skipped.
                val names = values.keys().asSequence().toList()
                for (k in names) {
                    if (isEffortMemoryKey(k) && values.opt(k) is String) {
                        putString(k, values.optString(k, ""))
                        applied++
                    }
                }
                // Per-widget display settings (#169): the export allowlist
                // mirrored per prefix — view/density take strings only,
                // due/scroll take booleans only. A cross-typed value in a
                // hand-edited backup (e.g. {"task_widget_due_12": "yes"})
                // is skipped, because the widget reads those keys with
                // getBoolean and a stored String would throw there.
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
            if (files != null) {
                for (name in backupFiles()) {
                    val text = files.optString(name, "")
                    if (text.isEmpty()) continue
                    if (name.startsWith("chat_threads_")) {
                        // Single parse, not probe-then-parse (#339): the old
                        // org.json JSONArray/JSONObject probe decoded the
                        // whole multi-MB string just to check shape, then
                        // canonicalizeFile decoded it AGAIN via kotlinx —
                        // 3x transient (probe + canonical + write) on the
                        // exact path the hard cap exists to bound.
                        // canonicalizeFile returns null on any invalid shape
                        // (decode failure → getOrNull), so the null IS the
                        // validity check with the same skip contract and one
                        // fewer full parse.
                        // Thread files canonicalize payloads first (#339):
                        // a hand-edited backup must not persist wrapped or
                        // ragged base64 that the load boundary would then
                        // have to heal. Valid-JSON-but-wrong-shape SKIPS for
                        // chat_threads_* (#339): the old raw fallback wrote
                        // bytes load() cannot decode, so the next load
                        // returned emptyList() — wiping the LIVE threads the
                        // import just overwrote. A skip keeps them; the count
                        // still hints at it (applied excludes the file).
                        // Fresh-reinstall loss is identical either way (the
                        // shape was never loadable).
                        // Oversize persists until send, BY DESIGN (#339):
                        // canonicalize strips whitespace and blanks ragged
                        // payloads but does NOT enforce the 1.5M staging cap
                        // — an old thread must stay readable even if a newer
                        // build would refuse to stage it. So a 3 MB imported
                        // payload loads, titles, and searches as a photo,
                        // then trips the named per-photo wire cap at send
                        // time. Fail-closed and correct; the failure just
                        // surfaces far from the cause, which this note —
                        // and the named error itself — closes.
                        val toWrite = ChatThreads.canonicalizeFile(text) ?: continue
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(toWrite, Charsets.UTF_8)
                            applied++
                        }
                        continue
                    }
                    // Validate JSON shape before overwriting live state.
                    // Non-thread files only: thread files validate via the
                    // canonicalizeFile null above (no decoder to disagree
                    // with here, so the org.json probe stays the check).
                    val valid = runCatching { org.json.JSONArray(text); true }
                        .recoverCatching { org.json.JSONObject(text); true }
                        .getOrDefault(false)
                    if (valid) {
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(text, Charsets.UTF_8)
                            applied++
                        }
                    }
                }
            }
            Result.success(applied)
        } catch (e: Exception) {
            Result.failure(e)
        }
    }
}
