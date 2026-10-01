package com.vishnu.agento

/**
 * How one task-surface refresh ended, for whoever scheduled it (#170).
 *
 * This is the shared vocabulary between the three things that refresh
 * surfaces: the widget, the reminder engine, and the periodic job that
 * asks both of them. It lives here rather than in either producer, so
 * neither producer is the authority on a word they both owe the scheduler.
 *
 * Two entries are success in the only sense that matters to a caller —
 * "nothing to retry" — and one is not: a run that ended unable to reach
 * the server is the one [TaskSyncWorker] backs off and retries.
 */
enum class RefreshOutcome {
    /** No placement is installed: nothing to fetch for, and not a failure. */
    NoPlacements,

    /** At least one state was fetched and pushed, or alarms re-derived. */
    Ok,

    /** Every state failed: surfaces are showing whatever they had cached. */
    Failed,
}
