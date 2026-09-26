package com.vishnu.agento

import android.content.Context
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
    val BOOLEAN_KEYS: List<String> = listOf("first_sync_done", "notify_reply_done")

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
            val edit = prefs.edit()
            var applied = 0
            for (k in STRING_KEYS) {
                if (values.has(k) && values.opt(k) is String) {
                    edit.putString(k, values.optString(k, ""))
                    applied++
                }
            }
            for (k in BOOLEAN_KEYS) {
                if (values.has(k) && values.opt(k) is Boolean) {
                    edit.putBoolean(k, values.optBoolean(k))
                    applied++
                }
            }
            // Real token totals: JSON numbers only (optLong coerces, so the
            // raw type is checked first like the other allowlists).
            // Negatives (hand-edited files) clamp to 0 — totals never go below.
            for (k in LONG_KEYS) {
                if (values.has(k) && values.opt(k) is Number) {
                    edit.putLong(k, maxOf(0L, values.optLong(k, 0L)))
                    applied++
                }
            }
            // Per-model effort memory: same allowlist shape as export
            // (isEffortMemoryKey, string values only). Unknown keys and
            // mistyped values are still skipped.
            val names = values.keys().asSequence().toList()
            for (k in names) {
                if (isEffortMemoryKey(k) && values.opt(k) is String) {
                    edit.putString(k, values.optString(k, ""))
                    applied++
                }
            }
            edit.apply()
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
