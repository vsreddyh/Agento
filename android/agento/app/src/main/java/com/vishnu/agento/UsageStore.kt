package com.vishnu.agento

import android.content.Context

/** Cumulative real token counts for one assistant tab. */
data class UsageTotals(
    val prompt: Long = 0L,
    val completion: Long = 0L,
    val total: Long = 0L,
    /** Turns whose stream reported usable counts (failed/zero turns excluded). */
    val turns: Long = 0L,
) {
    operator fun plus(other: UsageTotals) = UsageTotals(
        prompt = prompt + other.prompt,
        completion = completion + other.completion,
        total = total + other.total,
        turns = turns + other.turns,
    )
}

/**
 * Real token totals parsed from each turn's SSE `usage` object (see
 * [parseTokenUsage]). One cumulative row per assistant tab; counts only move
 * when the server reports usable numbers, so pre-update turns and failed
 * turns simply contribute nothing. The legacy character-counter keys
 * (`usage_sent_*`/`usage_recv_*`) are no longer written; [clearLegacy]
 * removes them once so stale estimates can't resurface.
 */
object UsageStore {

    private val TABS = listOf("god", "story", "resumes")

    private fun promptKey(tab: String) = "usage_prompt_$tab"
    private fun completionKey(tab: String) = "usage_completion_$tab"
    private fun totalKey(tab: String) = "usage_total_$tab"
    private fun turnsKey(tab: String) = "usage_turns_$tab"

    /** Every long pref this store reads; allowlisted for backup export/import. */
    val LONG_KEYS: List<String> = TABS.flatMap {
        listOf(promptKey(it), completionKey(it), totalKey(it), turnsKey(it))
    }

    private fun prefs(context: Context) =
        context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

    fun load(context: Context, tab: String): UsageTotals {
        val p = prefs(context)
        return UsageTotals(
            prompt = p.getLong(promptKey(tab), 0L),
            completion = p.getLong(completionKey(tab), 0L),
            total = p.getLong(totalKey(tab), 0L),
            turns = p.getLong(turnsKey(tab), 0L),
        )
    }

    fun loadAll(context: Context): Map<String, UsageTotals> =
        TABS.associateWith { load(context, it) }

    /** Adds one turn's counts; no-op when [usage] carries nothing usable. */
    fun add(context: Context, tab: String, usage: TokenUsage) {
        if (usage.prompt <= 0 && usage.completion <= 0 && usage.total <= 0) return
        val p = prefs(context)
        p.edit()
            .putLong(promptKey(tab), p.getLong(promptKey(tab), 0L) + usage.prompt)
            .putLong(completionKey(tab), p.getLong(completionKey(tab), 0L) + usage.completion)
            .putLong(totalKey(tab), p.getLong(totalKey(tab), 0L) + usage.total)
            .putLong(turnsKey(tab), p.getLong(turnsKey(tab), 0L) + 1)
            .apply()
    }

    /** Drops the retired character-estimate keys so only real counts remain. */
    fun clearLegacy(context: Context) {
        val p = prefs(context)
        val edit = p.edit()
        var dirty = false
        for (t in TABS) {
            for (k in listOf("usage_sent_$t", "usage_recv_$t")) {
                if (p.contains(k)) {
                    edit.remove(k)
                    dirty = true
                }
            }
        }
        if (dirty) edit.apply()
    }
}
