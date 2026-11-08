package com.vishnu.agento

import android.content.SharedPreferences

/**
 * The one endpoint rule for every gateway client (#398, #433).
 *
 * Tasks, projects, reports, storage and sync all resolve the server URL and
 * password through here. One canonical key each, and no retired fallbacks:
 * a config that still holds only `server_url`/`api_base_url`/`auth_token`
 * reads as unconfigured, so the caller can say so loudly instead of silently
 * talking to a stale server with the wrong credential.
 *
 * Keeping the rule in one place is the point — a retired key cannot land in
 * one client and be missed by another, and a future key has exactly one
 * reader to update.
 */
internal object ServerEndpoint {

    /** Canonical server URL pref: the proxy root that fronts sync + tasks. */
    const val BASE_KEY = "server_base_url"

    /** Canonical credential pref: the single app password. */
    const val PASSWORD_KEY = "app_password"

    /** The one loud, actionable "not configured" message (#433): a config
     * that only ever held a retired key must point the user at the fix
     * instead of failing with a bare exception or reading as healthy. */
    const val UNCONFIGURED = "Server URL not configured — re-enter it in Settings → Connection."

    /** Trimmed, trailing-slash-free base URL, or "" when unconfigured. */
    fun base(prefs: SharedPreferences): String =
        (prefs.getString(BASE_KEY, "") ?: "").trim().trimEnd('/')

    /** The single app password, or "" when unconfigured. */
    fun password(prefs: SharedPreferences): String =
        (prefs.getString(PASSWORD_KEY, "") ?: "").trim()
}
