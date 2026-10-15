package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import androidx.core.app.RemoteInput
import androidx.core.content.edit
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

        /** Thread id the chat tab should auto-send on open (single-shot). */
        const val PREF_INSTRUCT_AUTOSEND = "chat_instruct_autosend"
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
        val name = task?.name.orEmpty().ifEmpty { "Task" }
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
        // Single-shot auto-send flag, consumed by ChatViewModel.openThread.
        // Guarded there (user-only tail, not streaming), so a stale flag
        // can never fire into an ongoing turn.
        appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
            .edit {
                putString(PREF_INSTRUCT_AUTOSEND, thread.id)
            }
        ChatNotifications.notifyInstructSaved(appCtx, name, thread.id)
    }
}
