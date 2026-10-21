package com.vishnu.agento

import android.content.Context
import androidx.core.content.edit
import org.json.JSONObject

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
        // byte-identical and can be diffed.
        for (k in prefs.all.keys.filter { isWidgetDisplayKey(it) }.sorted()) {
            when (val v = prefs.all[k]) {
                is String -> values.put(k, v)
                is Boolean -> values.put(k, v)
            }
        }
        for (k in BOOLEAN_KEYS) {
            (all[k] as? Boolean)?.let { values.put(k, it) }
        }
        for (k in LONG_KEYS) {
            // Number, not Long (#339): a hand-set Int still carries the
            // count — toLong keeps it, maxOf drops impossible negatives,
            // mirroring the import clamp. Anything else skips.
            (all[k] as? Number)?.let { values.put(k, maxOf(0L, it.toLong())) }
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
                runCatching {
                    files.put(name, f.readText())
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
                    // Validate JSON shape before overwriting live state.
                    val valid = runCatching { org.json.JSONArray(text); true }
                        .recoverCatching { org.json.JSONObject(text); true }
                        .getOrDefault(false)
                    if (valid) {
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
                        // shape was never loadable). Non-thread files keep
                        // the raw write-through (no decoder to disagree with).
                        val toWrite = if (name.startsWith("chat_threads_")) {
                            ChatThreads.canonicalizeFile(text) ?: continue
                        } else {
                            text
                        }
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(toWrite)
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
