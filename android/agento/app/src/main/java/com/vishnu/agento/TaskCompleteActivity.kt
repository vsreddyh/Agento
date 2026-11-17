package com.vishnu.agento

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch

/**
 * Invisible trampoline for the row taps of the legacy service-backed
 * collection ([TaskWidgetService], API < 31), whose ListView has ONE
 * PendingIntent template for the whole list — so the row's open action
 * forces an activity target and the ring/skip share it. Placements built
 * from explicit intents (direct collection items on 31+, and the plain-rows
 * layout everywhere) send complete/skip as broadcasts straight to
 * [TaskNotifActionReceiver] (#532) and only open the detail sheet through
 * an activity, which is why a ring tap no longer foregrounds the app.
 *
 * Per-row fill-ins say what to do:
 *
 * - EXTRA_COMPLETE_ID → complete the task via TasksApi, refresh the
 *   widget, finish. The app never visibly opens: inline complete.
 * - EXTRA_SKIP_ID → the same for skip (no reason; the API accepts an empty
 *   one).
 * - EXTRA_TASK_ID → forward to MainActivity (foreground by now, so the
 *   start is allowed) with the deep-link extra, finish. The Task Manager
 *   opens the task's detail sheet.
 */
class TaskCompleteActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val completeId = intent.getStringExtra(TaskWidget.EXTRA_COMPLETE_ID)
            ?.takeIf { it.isNotEmpty() }
        val skipId = intent.getStringExtra(TaskNotifActionReceiver.EXTRA_SKIP_ID)
            ?.takeIf { it.isNotEmpty() }
        val openId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)
        val mutateId = completeId ?: skipId
        if (mutateId != null) {
            // completeId wins when both are present (they never are): a
            // fill-in carries one verb, and a corrupt intent must not skip
            // a task the user meant to complete.
            val skip = completeId == null
            lifecycleScope.launch {
                // getOrThrow inside the runCatching: complete()/skip() return
                // a Result, which never throws on API failure — without it
                // this is success even when the mutation failed, and the
                // alert is dismissed despite the comment below saying
                // otherwise.
                val done = runCatching {
                    val api = TasksApi(this@TaskCompleteActivity)
                    (if (skip) api.skip(mutateId) else api.complete(mutateId)).getOrThrow()
                }.isSuccess
                // Action taps dismiss every posted alert for the task (all
                // four reminder-point tags, #287) — but only on success, so
                // an offline tap keeps the alert.
                if (done) {
                    runCatching { TaskReminders.cancelPosted(this@TaskCompleteActivity, mutateId) }
                }
                // Join before finishing: the scopes are static and would
                // survive, but a process death right after the tap must not
                // lose the widget/alarm write. Awaited, not joined: the
                // Deferreds never fail, but an unexpected throw must be
                // observed rather than left in a Deferred nobody reads.
                awaitAll(
                    TaskReminders.refresh(this@TaskCompleteActivity),
                    TaskWidget.refresh(this@TaskCompleteActivity),
                )
                finish()
            }
        } else {
            val open = Intent(this, MainActivity::class.java)
                .setAction(TaskWidget.ACTION_TASKS)
            if (!openId.isNullOrEmpty()) {
                open.putExtra(TaskWidget.EXTRA_TASK_ID, openId)
            }
            startActivity(open)
            finish()
        }
    }
}
