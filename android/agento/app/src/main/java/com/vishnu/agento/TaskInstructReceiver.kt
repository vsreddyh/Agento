package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import androidx.core.app.RemoteInput
import java.time.LocalDate
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** Deep-link into one chat thread (task-instruction tap, #287). */
internal object InstructIntents {
    const val ACTION_CHAT = "com.vishnu.agento.action.CHAT_THREAD"
    const val EXTRA_THREAD_ID = "com.vishnu.agento.extra.THREAD_ID"
    const val EXTRA_AUTOSEND = "com.vishnu.agento.extra.AUTOSEND"
}

private val instructScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

/**
 * The notification's `[Instruct Agent]` answer box (#287).
 *
 * Reads the RemoteInput text, starts a new `chat`-tab thread tagging the
 * task plus the instruction, and posts a tap-to-open notification: the
 * receiver cannot start the chat UI itself (background activity launch),
 * so opening happens through the notification tap, which auto-sends the
 * thread exactly once ([ChatViewModel.openThread]).
 */
class TaskInstructReceiver : BroadcastReceiver() {

    companion object {
        const val KEY_INSTRUCT = "agento.instruct"
    }

    override fun onReceive(context: Context, intent: Intent) {
        val taskId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID).orEmpty()
        if (taskId.isEmpty()) return
        val text = RemoteInput.getResultsFromIntent(intent)
            ?.getCharSequence(KEY_INSTRUCT)?.toString().orEmpty().trim()
        // Empty send: nothing to start a chat with.
        if (text.isEmpty()) return
        val pending = goAsync()
        instructScope.launch {
            try {
                saveInstruction(context.applicationContext, taskId, text)
            } finally {
                pending.finish()
            }
        }
    }

    private suspend fun saveInstruction(appCtx: Context, taskId: String, instruction: String) {
        val task = withTimeoutOrNull(10_000) {
            TasksApi(appCtx).get(taskId)
        }?.getOrNull()
        // Unknown when the fetch missed (offline/timeout): fall back to the
        // raw id so the thread still links the task instead of naming "Task".
        val name = task?.name.orEmpty().ifEmpty { "task $taskId" }
        val dueLine = if (task != null) {
            friendlyDue(task.dueDate, task.dueTime, LocalDate.now(IST))
        } else {
            ""
        }
        val tag = if (dueLine.isNotEmpty()) {
            "Regarding task \"$name\" (due $dueLine)"
        } else {
            "Regarding task \"$name\""
        }
        val content = "$tag: $instruction"
        val now = ChatThreads.now()
        val thread = ChatThread(
            id = ChatThreads.newId(),
            title = ChatThreads.titleFor(listOf(ChatMessage("user", content, now))),
            updatedAt = now,
            messages = ChatThreads.toStored(listOf(ChatMessage("user", content, now))),
        )
        val threads = listOf(thread) + ChatThreads.load(appCtx, "chat")
            .filter { it.id.isNotEmpty() && it.id != thread.id }
        ChatThreads.save(appCtx, "chat", threads)
        // The tap-to-open card carries the thread id + autosend intent
        // (InstructIntents extras), so no persisted flag is needed: the
        // receiver-to-chat path is the notification tap itself.
        ChatNotifications.notifyInstructSaved(appCtx, name, thread.id)
    }
}
