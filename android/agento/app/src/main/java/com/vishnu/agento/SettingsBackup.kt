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

    // NOTE: stays 1 — the prefs schema gains only additive optional keys
    // (usage totals, per-model effort memory) and old readers ignore the
    // additive "files" section, so no MAJOR bump is required.
    const val VERSION = 1

    private val TABS = listOf("story", "resumes", "god")

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
     * Dynamic per-model effort memory keys: present in prefs but unknowable
     * statically, so they are enumerated live. Only string values are exported.
     */
    private fun effortKeys(prefs: android.content.SharedPreferences): List<String> =
        prefs.all.keys.filter { k ->
            isEffortMemoryKey(k) && prefs.all[k] is String
        }.sorted()

    /**
     * Per-widget display settings, keyed by the widget id the user configured:
     * `task_widget_{view,density,due,scroll}_<appWidgetId>`, written by
     * `TaskWidgetConfigActivity.onSave` and read by the widget rows (#169).
     *
     * Same shape as the effort-memory keys — enumerated live because the
     * suffix is a runtime id, and shaped tight so a hand-edited backup cannot
     * inject arbitrary `task_widget_*` prefs. Values are mixed: `view` and
     * `density` are enum names, `due` and `scroll` are booleans, so export
     * writes each by its own type.
     *
     * Deliberately NOT backed up, and the reason matters: the two other
     * `task_reminder_*` prefs are device-local alarm bookkeeping, not settings.
     * `task_reminder_armed` is the set of currently-scheduled alarms — restoring
     * it would make a new device believe alarms it never scheduled are armed,
     * and the refresh diff would skip arming them. `task_reminder_budget` is the
     * last refresh's six counts, recomputed on every refresh. The one reminder
     * key that is a setting is `task_reminders_enabled` in [BOOLEAN_KEYS].
     */
    private val WIDGET_PREFIXES = listOf(
        "task_widget_view_",
        "task_widget_density_",
        "task_widget_due_",
        "task_widget_scroll_",
    )

    /** True for a per-widget display key `task_widget_{view,density,due,scroll}_<id>`. */
    fun isWidgetDisplayKey(k: String): Boolean =
        WIDGET_PREFIXES.any { k.startsWith(it) && k.length > it.length }

    /** Serializes all known prefs plus app files; absent keys are omitted (not nulled). */
    fun export(context: Context): JSONObject {
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        val values = JSONObject()
        for (k in STRING_KEYS) {
            if (prefs.contains(k)) values.put(k, prefs.getString(k, "") ?: "")
        }
        for (k in effortKeys(prefs)) {
            values.put(k, prefs.getString(k, "") ?: "")
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
            if (prefs.contains(k)) values.put(k, prefs.getBoolean(k, false))
        }
        for (k in LONG_KEYS) {
            if (prefs.contains(k)) values.put(k, prefs.getLong(k, 0L))
        }
        val files = JSONObject()
        for (name in backupFiles()) {
            val f = java.io.File(context.filesDir, name)
            if (f.exists()) {
                runCatching { files.put(name, f.readText()) }
            }
        }
        return JSONObject().put("version", VERSION).put("values", values).put("files", files)
    }

    /**
     * Applies a backup produced by [export]; unknown keys and mistyped values
     * are skipped. Version 1 backups (prefs only) still import. Returns the
     * number of prefs + files applied.
     */
    fun importFrom(context: Context, json: JSONObject): Result<Int> {
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
                // mirrored — string fields (view/density) and boolean fields
                // (due/scroll), nothing else. A mistyped value in a
                // hand-edited backup is skipped rather than coerced, exactly
                // like every other key above.
                for (k in names) {
                    if (!isWidgetDisplayKey(k)) continue
                    if (values.opt(k) is String) {
                        putString(k, values.optString(k, ""))
                        applied++
                    } else if (values.opt(k) is Boolean) {
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
                        runCatching {
                            java.io.File(context.filesDir, name).writeText(text)
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
