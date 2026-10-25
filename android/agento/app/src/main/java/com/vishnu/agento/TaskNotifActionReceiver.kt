package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** Which task mutation a notification tap asked for (#368). */
internal enum class NotifAction {
    COMPLETE,
    SKIP,
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
         */
        const val EXTRA_SKIP_ID = "com.vishnu.agento.extra.SKIP_ID"
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
        val appCtx = context.applicationContext
        val pending = goAsync()
        notifActionScope.launch {
            try {
                withTimeoutOrNull(60_000) {
                    when (target) {
                        NotifAction.COMPLETE -> {
                            val id = intent.getStringExtra(TaskWidget.EXTRA_COMPLETE_ID).orEmpty()
                            if (id.isNotEmpty()) {
                                // getOrThrow inside the runCatching: complete()
                                // returns a Result, which never throws on API
                                // failure — without it this is success even
                                // when the completion failed (same shape as
                                // the trampoline it replaces).
                                val done = runCatching {
                                    TasksApi(appCtx).complete(id).getOrThrow()
                                }.isSuccess
                                if (done) {
                                    runCatching { TaskReminders.cancelPosted(appCtx, id) }
                                }
                                refreshSurfaces(appCtx)
                            }
                        }
                        NotifAction.SKIP -> {
                            val id = intent.getStringExtra(EXTRA_SKIP_ID).orEmpty()
                            if (id.isNotEmpty()) {
                                val skipped = runCatching {
                                    TasksApi(appCtx).skip(id).getOrThrow()
                                }.isSuccess
                                if (skipped) {
                                    runCatching { TaskReminders.cancelPosted(appCtx, id) }
                                }
                                refreshSurfaces(appCtx)
                            }
                        }
                    }
                }
            } finally {
                pending.finish()
            }
        }
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
