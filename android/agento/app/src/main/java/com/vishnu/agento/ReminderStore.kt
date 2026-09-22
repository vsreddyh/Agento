package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One reminder: fires a local notification at [atEpoch] (#31, #32). */
@Serializable
data class ReminderItem(
    val id: String = "",
    val title: String = "",
    val text: String = "",
    val atEpoch: Long = 0L,
)

private val reminderJson = Json { ignoreUnknownKeys = true }

/** App-local reminders persisted as JSON (#31, #32). */
object ReminderStore {

    const val MAX_REMINDERS = 100

    private fun file(context: Context): File = File(context.filesDir, "reminders.json")

    fun newId(): String = java.util.UUID.randomUUID().toString()

    fun load(context: Context): List<ReminderItem> {
        val f = file(context)
        if (!f.exists()) return emptyList()
        return runCatching {
            reminderJson.decodeFromString(ListSerializer(ReminderItem.serializer()), f.readText())
                .filter { it.id.isNotEmpty() }
                .take(MAX_REMINDERS)
        }.getOrDefault(emptyList())
    }

    fun save(context: Context, items: List<ReminderItem>) {
        runCatching {
            file(context).writeText(reminderJson.encodeToString(ListSerializer(ReminderItem.serializer()), items.take(MAX_REMINDERS)))
        }
    }
}
