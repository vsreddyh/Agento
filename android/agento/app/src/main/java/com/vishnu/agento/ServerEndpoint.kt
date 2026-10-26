package com.vishnu.agento

import android.content.SharedPreferences

/**
 * One resolution rule for the gateway endpoint (#398).
 *
 * Chat, questions and every other typed client read the same two prefs in
 * the same order; keeping the chain in one place means a new fallback key —
 * or the removal of a retired one — cannot land in one client and be missed
 * by another, which is how `QuestionsApi` drifted from `ChatApi`.
 */
internal object ServerEndpoint {

    /** Unification chain, in priority order: the single server URL the app
     * saves first, then the legacy keys a pre-unification config may still
     * hold. Removed on save, so a normal install only ever has the first. */
    private val BASE_KEYS = listOf("server_base_url", "server_url", "api_base_url")

    /** Trimmed, trailing-slash-free base URL, or "" when unconfigured. */
    fun base(prefs: SharedPreferences): String {
        for (k in BASE_KEYS) {
            val v = (prefs.getString(k, "") ?: "").trim().trimEnd('/')
            if (v.isNotEmpty()) return v
        }
        return ""
    }

    /** The single app password (the sync token doubles as the chat
     * credential). Prefers `app_password`; falls back to the legacy
     * `auth_token` so users who configured a sync token keep working without
     * re-entry. The retired `api_key` is deliberately NOT read (the server no
     * longer accepts it). */
    fun password(prefs: SharedPreferences): String {
        val v = (prefs.getString("app_password", "") ?: "").trim()
        if (v.isNotEmpty()) return v
        return (prefs.getString("auth_token", "") ?: "").trim()
    }
}
