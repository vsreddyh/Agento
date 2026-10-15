package com.vishnu.agento

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch

/**
 * Invisible trampoline for the reminder notification's `[Skip]` action
 * (#287): skips the current occurrence without claiming the work was done
 * (Theme.Agento.Trampoline, noHistory, excluded from recents).
 *
 * Same shape as [TaskCompleteActivity]: the app never visibly opens. On a
 * successful skip every posted alert for the task is dismissed
 * ([TaskReminders.cancelPosted]) so an offline tap keeps the alert while a
 * completed one clears all four reminder points at once.
 */
class TaskSkipActivity : ComponentActivity() {

    companion object {
        const val EXTRA_SKIP_ID = "com.vishnu.agento.extra.SKIP_ID"
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val skipId = intent.getStringExtra(EXTRA_SKIP_ID)
        if (!skipId.isNullOrEmpty()) {
            lifecycleScope.launch {
                // getOrThrow inside the runCatching: skip() returns a
                // Result, which never throws on API failure — without it
                // this is success even when the skip failed, and the alert
                // is dismissed despite the comment below saying otherwise.
                val skipped =
                    runCatching { TasksApi(this@TaskSkipActivity).skip(skipId).getOrThrow() }.isSuccess
                if (skipped) {
                    runCatching { TaskReminders.cancelPosted(this@TaskSkipActivity, skipId) }
                }
                // Join before finishing: the scopes are static and would
                // survive, but a process death right after the tap must
                // not lose the widget/alarm write. Awaited, not joined:
                // the Deferreds never fail, but an unexpected throw must
                // be observed rather than left in a Deferred nobody reads.
                awaitAll(
                    TaskReminders.refresh(this@TaskSkipActivity),
                    TaskWidget.refresh(this@TaskSkipActivity),
                )
                finish()
            }
        } else {
            finish()
        }
    }
}
