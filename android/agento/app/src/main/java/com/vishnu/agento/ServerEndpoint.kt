package com.vishnu.agento

import android.content.SharedPreferences

/**
 * One resolution rule for the gateway endpoint (#398, canonical-only #433).
 *
 * Single canonical keys: `server_base_url` and `app_password`. Retired keys
 * (`server_url`, `api_base_url`, `auth_token`) are NOT read — an install
 * holding only retired keys resolves to "" and every caller fails loudly
 * with its "not configured" error, so the user re-enters config once
 * instead of silently talking to the wrong server with the wrong
 * credential. The retired `api_key` was never read and stays unread.
 */
internal object ServerEndpoint {

    /** Trimmed, trailing-slash-free base URL, or "" when unconfigured. */
    fun base(prefs: SharedPreferences): String =
        (prefs.getString("server_base_url", "") ?: "").trim().trimEnd('/')

    /** The single app password (the sync token doubles as the chat
     * credential), or "" when unconfigured. */
    fun password(prefs: SharedPreferences): String =
        (prefs.getString("app_password", "") ?: "").trim()
}
