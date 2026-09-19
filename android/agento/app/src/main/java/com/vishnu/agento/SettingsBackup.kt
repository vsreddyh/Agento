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

    const val VERSION = 1

    private val TABS = listOf("story", "resumes", "god")

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
        }
        add("server_url")
        add("auth_token")
        add("last_sync_at")
    }

    /** Boolean prefs the app reads. */
    val BOOLEAN_KEYS: List<String> = listOf("first_sync_done")

    /** Serializes all known prefs; absent keys are omitted (not nulled). */
    fun export(context: Context): JSONObject {
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        val values = JSONObject()
        for (k in STRING_KEYS) {
            if (prefs.contains(k)) values.put(k, prefs.getString(k, "") ?: "")
        }
        for (k in BOOLEAN_KEYS) {
            if (prefs.contains(k)) values.put(k, prefs.getBoolean(k, false))
        }
        return JSONObject().put("version", VERSION).put("values", values)
    }

    /**
     * Applies a backup produced by [export]; unknown keys and mistyped values
     * are skipped. Returns the number of prefs applied.
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
            edit.apply()
            Result.success(applied)
        } catch (e: Exception) {
            Result.failure(e)
        }
    }
}
