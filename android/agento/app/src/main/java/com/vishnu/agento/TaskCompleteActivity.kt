package com.vishnu.agento

import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch

/**
 * Invisible trampoline for task-widget row taps (Theme.Agento.Trampoline,
 * noHistory, excluded from recents). Widget taps must stay BAL-safe, so
 * rows can't complete-then-open from a broadcast: instead the collection
 * template points here and per-row fill-ins say what to do —
 *
 * - EXTRA_COMPLETE_ID → complete the task via TasksApi, refresh the
 *   widget, finish. The app never visibly opens: inline complete.
 * - EXTRA_TASK_ID → forward to MainActivity (foreground by now, so the
 *   start is allowed) with the deep-link extra, finish. The Task Manager
 *   opens the task's detail sheet.
 */
class TaskCompleteActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val completeId = intent.getStringExtra(TaskWidget.EXTRA_COMPLETE_ID)
        val openId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)
        if (!completeId.isNullOrEmpty()) {
            lifecycleScope.launch {
                runCatching { TasksApi(this@TaskCompleteActivity).complete(completeId) }
                // Action taps don't auto-cancel: dismiss the due alert
                // this completion came from (tag + id match the post).
                runCatching {
                    (getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager)
                        .cancel(completeId, ALARM_NOTIF_ID)
                }
                // Join before finishing: the scopes are static and would
                // survive, but a process death right after the tap must
                // not lose the widget/alarm write.
                joinAll(
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
