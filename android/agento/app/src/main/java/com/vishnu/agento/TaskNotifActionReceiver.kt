package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** Which task mutation a notification tap asked for (#368). */
internal enum class NotifAction(
    /** Extra that carries the task id for this action. Kept on the target so
     * onReceive reads one extra instead of branching per action (#386). */
    val extraKey: String,
) {
    COMPLETE(TaskWidget.EXTRA_COMPLETE_ID),
    SKIP(TaskNotifActionReceiver.EXTRA_SKIP_ID),
}

private val notifActionScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

/**
 * Notification-action taps for task reminders (#368): `[Completed]` and
 * `[Skip]` complete inline in the background, with the app never visibly
 * opening.
 *
 * These used to be `PendingIntent.getActivity()` calls into the widget
 * complete trampoline ([TaskCompleteActivity]) and a dedicated skip
 * trampoline. On Android 12+ starting an activity from a notification
 * brings the task to the foreground even through a translucent noHistory
 * trampoline — the tap visibly opened the app. A broadcast fired from our
 * own PendingIntent needs no activity at all (unlike the widget rows,
 * which keep their trampoline for background-launch reasons), so the
 * actions go straight here instead.
 *
 * Semantics match the trampoline exactly: on success every posted alert
 * for the task is dismissed ([TaskReminders.cancelPosted]) and both
 * surfaces refresh; on failure the alert stays, so an offline tap keeps
 * the reminder instead of silently dropping it.
 */
class TaskNotifActionReceiver : BroadcastReceiver() {

    companion object {
        const val ACTION_COMPLETE = "com.vishnu.agento.action.NOTIF_COMPLETE"
        const val ACTION_SKIP = "com.vishnu.agento.action.NOTIF_SKIP"

        /**
         * Skip target, read from the skip intent. Same string the retired
         * skip trampoline used (`com.vishnu.agento.extra.SKIP_ID`), so a
         * spare intent built against the old key still resolves — and, like
         * the old manifest entry, this receiver stays non-exported so no
         * outside app can guess an ObjectID into a skip.
         *
         * Known one-release skew (#410): alerts posted by 5.14.0 carry a
         * Skip PendingIntent targeting the deleted `.TaskSkipActivity`, so
         * tapping one before the next alarm reposts it no-ops. The repost
         * heals it; Complete taps from that build still resolve because
         * [TaskCompleteActivity] was kept.
         */
        const val EXTRA_SKIP_ID = "com.vishnu.agento.extra.SKIP_ID"

        /**
         * How long a tap waits for its mutation and refresh (#384). A
         * background broadcast gets roughly 10-30 s of wake lock, so the old
         * 60 s ceiling could only ever be hit from the wrong side: the system
         * cut the work first, invisibly. Waiting inside that budget means the
         * timeout, when it fires, is ours and logged.
         *
         * The timeout cancels the in-flight mutation POST (a cancellable
         * OkHttp call — safe: failure keeps the alert, so the next tap
         * retries). Only the refresh half outlives it: [handleMutation]
         * awaits Deferreds owned by other scopes, so a timeout stops
         * *waiting* while that work keeps running without the wake lock —
         * process death there can lose it, the same caveat the old
         * trampoline carried.
         */
        private const val MUTATION_TIMEOUT_MS = 12_000L
    }

    /**
     * Routes an incoming action string. Unknown (or null) actions are
     * caller errors or version skew — never a task mutation, so they map
     * to null and onReceive does nothing (not even a surface refresh).
     * Pure, JVM-pinned: the wire strings below are a compat surface, and
     * a rename that orphans an already-posted notification's tap must be
     * a deliberate, reviewed change.
     */
    internal fun notifActionFor(action: String?): NotifAction? = when (action) {
        ACTION_COMPLETE -> NotifAction.COMPLETE
        ACTION_SKIP -> NotifAction.SKIP
        else -> null
    }

    override fun onReceive(context: Context, intent: Intent) {
        // Routed once, up front: unknown actions return before goAsync,
        // so a stray tap costs neither a wake lock nor a surface refresh.
        val target = notifActionFor(intent.action) ?: return
        // Read before goAsync and before any coroutine (#386): a malformed
        // tap is then fully free — no wake lock, no launch — and no Intent
        // field is touched off the Main thread. One key per action, held on
        // the target, so the two branches cannot drift apart.
        val id = intent.getStringExtra(target.extraKey).orEmpty()
        // Empty id does nothing at all — deliberately unlike the trampoline
        // this replaces (#385): `TaskCompleteActivity` fell through to
        // opening MainActivity when neither extra was present, but a corrupt
        // or spoofed tap must not foreground the app. There is no task to
        // mutate and no surface worth refreshing, so return — loudly: the
        // action name (no id, nothing to leak) tells corrupt taps apart
        // from version skew in the field.
        if (id.isEmpty()) {
            Log.w("TaskNotif", "notification $target with no id ignored")
            return
        }
        val appCtx = context.applicationContext
        val pending = goAsync()
        notifActionScope.launch {
            try {
                val finished = withTimeoutOrNull(MUTATION_TIMEOUT_MS) {
                    handleMutation(appCtx, id, target)
                }
                // Never silent (#384): a tap that ran out of broadcast budget
                // used to vanish, leaving an alert that looked handled. The
                // mutation POST is cancelled (failure keeps the alert, so
                // the next tap retries); only the refresh half runs on
                // without the wake lock.
                if (finished == null) {
                    Log.w(
                        "TaskNotif",
                        "notification $target for $id timed out after " +
                            "${MUTATION_TIMEOUT_MS}ms; the mutation was cancelled " +
                            "and the refresh continues without the wake lock",
                    )
                }
            } finally {
                pending.finish()
            }
        }
    }

    /**
     * One mutation, shared by both actions (#387): COMPLETE and SKIP differed
     * only in which call they made and which extra they read, so a fix that
     * landed on one branch was invisible in the other.
     *
     * Semantics unchanged from the retired trampoline: success dismisses
     * every posted alert for the task and refreshes both surfaces; failure
     * keeps the alert, so an offline tap keeps the reminder rather than
     * silently dropping it.
     */
    private suspend fun handleMutation(appCtx: Context, id: String, target: NotifAction) {
        // getOrThrow inside the runCatching: complete()/skip() return a
        // Result, which never throws on API failure — without it this is
        // success even when the mutation failed (same shape as the
        // trampoline it replaces).
        val done = runCatching {
            val api = TasksApi(appCtx)
            when (target) {
                NotifAction.COMPLETE -> api.complete(id)
                NotifAction.SKIP -> api.skip(id)
            }.getOrThrow()
        }.isSuccess
        if (done) {
            runCatching { TaskReminders.cancelPosted(appCtx, id) }
        }
        refreshSurfaces(appCtx)
    }

    /**
     * Re-derives alarms and the widget after the mutation. Awaited, not
     * joined: the Deferreds never fail, but an unexpected throw must be
     * observed rather than left in a Deferred nobody reads (same shape as
     * the trampolines).
     */
    private suspend fun refreshSurfaces(appCtx: Context) {
        awaitAll(
            TaskReminders.refresh(appCtx),
            TaskWidget.refresh(appCtx),
        )
    }
}
