package com.vishnu.agento

import android.content.Context
import androidx.core.content.edit
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/**
 * Cron response discovery (#289): which stored runs are new since the last
 * check, and posting one notification per job for them.
 *
 * Seen state is device-local bookkeeping (`cron_seen_<jobId>` = newest run
 * instant observed), deliberately NOT part of Settings backup — same reason
 * as the reminder armed set (#169): restoring it would make a new device
 * believe runs it never saw are seen. A first check records without
 * notifying, so installing over an old history never blasts.
 */
internal object CronCheck {

    private fun seenKey(jobId: String) = "cron_seen_$jobId"

    /**
     * Runs newer than what was seen. Null `seenAt` (never checked) means
     * "record, don't notify" — the caller advances the cursor without
     * posting. Runs without an instant (`at` 0) are never "new": without a
     * timestamp they cannot be ordered, and notifying for them on every
     * check would nag forever.
     */
    internal fun unseenRuns(runs: List<CronRun>, seenAt: Long?): List<CronRun> {
        if (seenAt == null) return emptyList()
        return runs.filter { it.at > seenAt }
    }

    private fun prefs(context: Context) =
        context.applicationContext.getSharedPreferences(
            AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

    /**
     * Check stored jobs for fresh responses and notify once per job.
     * Suspends: must be called from a worker or a launched coroutine, never
     * the main thread bare. Returns how many notifications posted. Safe to
     * call from a worker or a screen refresh: failures are swallowed (a
     * missed check retries on the next tick). The cursor advances past
     * posted runs, plus kind-off and empty-text drops; only a denied
     * notification permission retries rather than dropping the runs.
     */
    suspend fun check(context: Context): Int {
        val appCtx = context.applicationContext
        val listed = withContext(Dispatchers.IO) {
            JobsApi(appCtx).list(ChatApi(appCtx).pathFor("chat")).getOrNull()
        }
        // Total failure reads as empty without this (#447 review): the
        // partial-drop path logs, so the total path must too — otherwise
        // "fail loudly" holds everywhere except here.
        if (listed == null) {
            runCatching {
                android.util.Log.w("CronCheck", "job list fetch failed, skipping check")
            }
            return 0
        }
        // Parse-skipped jobs never notify (#447 review): the runs below
        // only see what parsed, so log the count instead of rendering the
        // check complete.
        if (listed.dropped > 0) {
            runCatching {
                android.util.Log.w(
                    "CronCheck",
                    "dropped ${listed.dropped} unparseable job(s)",
                )
            }
        }
        return notifyFresh(appCtx, listed.jobs, listed.dropped)
    }

    /**
     * Cursor + notify pass over an already-fetched job list, so a screen
     * that just loaded does not fetch twice. Same rules as [check].
     */
    fun notifyFresh(context: Context, jobs: List<CronJob>, dropped: Int = 0): Int {
        val appCtx = context.applicationContext
        var posted = 0
        val p = prefs(appCtx)
        // A first sighting right after a drops episode ends is likely a
        // recovered job, not a new one (#447 review): baseline-silencing it
        // would lose the intervening runs to the fresh cursor. Treat its
        // history as fresh exactly once (latest run + count, one
        // notification — no blast). Scoped to the episode-ending clean
        // fetch, not the whole partial episode: newcomers arriving while
        // drops are still ongoing baseline silently like always, since a
        // still-dropping fetch can't tell them apart from recoveries. A
        // per-id scope would be precise, but parseJob fails only on an empty
        // id, so a dropped row never has an id to match on — the aggregate
        // flag is the only signal that exists.
        val prevDropped = p.getInt("cron_prev_dropped", 0)
        if (dropped != prevDropped) p.edit { putInt("cron_prev_dropped", dropped) }
        val recovered = prevDropped > 0 && dropped == 0
        for (job in jobs) {
            val has = p.contains(seenKey(job.id))
            val seen = p.getLong(seenKey(job.id), 0L)
            val fresh = if (!has && recovered) unseenRuns(job.runs, 0L)
                else unseenRuns(job.runs, if (has) seen else null)
            val maxAt = job.runs.maxOfOrNull { it.at } ?: 0L
            if (fresh.isEmpty()) {
                // First check (or nothing new): record the cursor so the
                // next check has a baseline. Nothing to notify.
                if (!has && maxAt > 0) {
                    p.edit { putLong(seenKey(job.id), maxAt) }
                }
                continue
            }
            val latest = fresh.maxByOrNull { it.at } ?: continue
            val more = fresh.size - 1
            val snippet = latest.text.replace(Regex("\\s+"), " ").trim().take(160)
            // Opted out means drop, not defer (#355): when the user turned
            // scheduler responses (or the master switch) off, runs from
            // while it was off are skipped — re-enabling surfaces only new
            // runs, never a catch-up blast. System denial still queues and
            // retries below: the user didn't opt out, the OS refused.
            //
            // notifyCron re-checks the same gate before posting: a flip
            // mid-loop only disagrees safely (check saw on, post sees off
            // → no post, cursor stays, retry next tick). The reverse needs
            // no handling: check-saw-off advances and continues before the
            // post is ever reached.
            if (!BehaviourSettings.notifyCron(appCtx)) {
                p.edit { putLong(seenKey(job.id), maxAt) }
                continue
            }
            // An empty-text run has nothing to show and would stall the
            // cursor forever (every check retries it, hiding later runs
            // behind it): advance past it without counting a post.
            if (snippet.isEmpty()) {
                p.edit { putLong(seenKey(job.id), maxAt) }
                continue
            }
            if (ChatNotifications.notifyCron(appCtx, job.id, job.name, snippet, more)
            ) {
                posted++
                p.edit { putLong(seenKey(job.id), maxAt) }
            }
        }
        return posted
    }
}
