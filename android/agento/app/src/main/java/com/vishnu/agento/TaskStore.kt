package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One task row: name + fixed status + note (#34, #55). Status is one of
 * Todo, Ongoing, Paused, Done (legacy free-text values migrate on load). */
@Serializable
data class TaskItem(
    val id: String = "",
    val name: String = "",
    val status: String = "todo",
    val note: String = "",
    val updatedAt: Long = 0L,
)

private val taskJson = Json { ignoreUnknownKeys = true }

/** App-local task table persisted as JSON (#34). */
object TaskStore {

    const val MAX_TASKS = 200

    private fun file(context: Context): File = File(context.filesDir, "tasks.json")

    fun newId(): String = java.util.UUID.randomUUID().toString()

    fun load(context: Context): List<TaskItem> {
        val f = file(context)
        if (!f.exists()) return emptyList()
        return runCatching {
            taskJson.decodeFromString(ListSerializer(TaskItem.serializer()), f.readText())
                .filter { it.id.isNotEmpty() }
                .take(MAX_TASKS)
        }.getOrDefault(emptyList())
    }

    fun save(context: Context, tasks: List<TaskItem>) {
        runCatching {
            file(context).writeText(taskJson.encodeToString(ListSerializer(TaskItem.serializer()), tasks.take(MAX_TASKS)))
        }
    }
}
