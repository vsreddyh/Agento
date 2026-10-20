package com.vishnu.agento

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

/**
 * Per-profile memory (#315).
 *
 * Pending proposals confirm into the next turn's prompt; confirmed rows edit
 * or delete; topics pause (kept on screen, skipped at injection); the
 * composer writes user items (confirmed on arrival). Forget wipes items but
 * keeps pauses. Every write disables its buttons while in flight so a double
 * tap cannot fire two writes.
 */
@Composable
internal fun MemoryDialog(
    items: List<MemoryItem>,
    paused: List<String>,
    topics: List<String>,
    warnings: List<String>,
    loading: Boolean,
    error: String,
    /** Write keys in flight — "item:\u2026" for rows, "topic:\u2026" for
     * pauses, bare "new"/"forget" for the composer and wipe. A listed
     * key disables its buttons so a double tap cannot fire two writes. */
    acting: Set<String> = emptySet(),
    onConfirm: (String) -> Unit,
    /** onSaved fires only on success: the editor collapses then, so a failed
     * save keeps the edit context alongside the error. */
    onEdit: (String, String, String, () -> Unit) -> Unit,
    onDelete: (String) -> Unit,
    onTogglePause: (String, Boolean) -> Unit,
    /** onSaved fires only on success: the composer clears then, so a failed
     * save keeps the typed text alongside the error. */
    onCreate: (String, String, () -> Unit) -> Unit,
    onForget: () -> Unit,
    onRefresh: () -> Unit,
    onClose: () -> Unit,
) {
    var editingId by remember { mutableStateOf<String?>(null) }
    var editText by remember { mutableStateOf("") }
    var editTopic by remember { mutableStateOf("") }
    var newTopic by remember { mutableStateOf("") }
    var newText by remember { mutableStateOf("") }
    var forgetArmed by remember { mutableStateOf(false) }
    // Arming is strictly one-shot: every load (start and landing)
    // disarms, so a stale arm can never meet a later tap and wipe without
    // its second confirm. Keyed on loading as well as content: an
    // identical-rows reload would otherwise not relaunch the effect.
    LaunchedEffect(items, loading) { forgetArmed = false }
    val pending = items.filter { it.isPending() }
    val confirmed = items.filter { !it.isPending() }
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Memory") },
        text = {
            Column(modifier = Modifier.fillMaxWidth()) {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.End,
                ) {
                    TextButton(
                        onClick = {
                            // Disarm here too: LaunchedEffect(items) only
                            // fires on content change, and a refresh returning
                            // identical rows would otherwise leave a stale arm
                            // meeting the next Forget tap.
                            forgetArmed = false
                            onRefresh()
                        },
                        enabled = !loading,
                    ) {
                        Text(if (loading) "Loading…" else "Refresh")
                    }
                }
                if (error.isNotBlank()) {
                    Text(
                        error,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.fillMaxWidth().padding(bottom = 8.dp),
                    )
                }
                for (warning in warnings) {
                    Text(
                        warning,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.tertiary,
                        modifier = Modifier.fillMaxWidth().padding(bottom = 8.dp),
                    )
                }
                if (loading && items.isEmpty()) {
                    Text(
                        "Loading…",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    LazyColumn(
                        // Bounded: an unbounded list inside a dialog can
                        // overflow or refuse to scroll on small screens.
                        modifier = Modifier.heightIn(max = 420.dp),
                        verticalArrangement = Arrangement.spacedBy(12.dp),
                    ) {
                        if (pending.isNotEmpty()) {
                            item {
                                Text(
                                    "Waiting for review (${pending.size})",
                                    style = MaterialTheme.typography.titleSmall,
                                )
                            }
                            items(pending, key = { it.id }) { item ->
                                MemoryRow(
                                    item = item,
                                    editing = editingId == item.id,
                                    editText = editText,
                                    editTopic = editTopic,
                                    onEditText = { editText = it },
                                    onEditTopic = { editTopic = it },
                                    busy = "item:${item.id}" in acting,
                                    onStartEdit = {
                                        editingId = item.id
                                        editText = item.text
                                        editTopic = item.topic
                                    },
                                    onCancelEdit = { editingId = null },
                                    onSaveEdit = {
                                        onEdit(item.id, editText, editTopic) {
                                            editingId = null
                                        }
                                    },
                                    onConfirm = { onConfirm(item.id) },
                                    onDelete = { onDelete(item.id) },
                                )
                            }
                        }
                        if (confirmed.isNotEmpty()) {
                            item {
                                Text(
                                    "Remembered",
                                    style = MaterialTheme.typography.titleSmall,
                                    modifier = Modifier.padding(top = if (pending.isEmpty()) 0.dp else 4.dp),
                                )
                            }
                            items(confirmed, key = { it.id }) { item ->
                                MemoryRow(
                                    item = item,
                                    editing = editingId == item.id,
                                    editText = editText,
                                    editTopic = editTopic,
                                    onEditText = { editText = it },
                                    onEditTopic = { editTopic = it },
                                    busy = "item:${item.id}" in acting,
                                    onStartEdit = {
                                        editingId = item.id
                                        editText = item.text
                                        editTopic = item.topic
                                    },
                                    onCancelEdit = { editingId = null },
                                    onSaveEdit = {
                                        onEdit(item.id, editText, editTopic) {
                                            editingId = null
                                        }
                                    },
                                    onConfirm = null,
                                    onDelete = { onDelete(item.id) },
                                )
                            }
                        }
                        if (pending.isEmpty() && confirmed.isEmpty()) {
                            item {
                                Text(
                                    "Nothing remembered yet.",
                                    style = MaterialTheme.typography.bodyMedium,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                        if (topics.isNotEmpty()) {
                            item {
                                Text(
                                    "Topics",
                                    style = MaterialTheme.typography.titleSmall,
                                    modifier = Modifier.padding(top = 4.dp),
                                )
                            }
                            items(topics.chunked(2), key = { it.joinToString("|") }) { pair ->
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                                ) {
                                    for (topic in pair) {
                                        val isPaused = topic in paused
                                        val busy = "topic:$topic" in acting
                                        TextButton(
                                            onClick = { onTogglePause(topic, !isPaused) },
                                            enabled = !busy,
                                            modifier = Modifier.weight(1f),
                                        ) {
                                            Text(
                                                (if (isPaused) "▶ " else "⏸ ") + topic,
                                                maxLines = 1,
                                                overflow = TextOverflow.Ellipsis,
                                            )
                                        }
                                    }
                                }
                            }
                            item {
                                Text(
                                    "Paused topics stay visible but are skipped in turns.",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                        item {
                            OutlinedTextField(
                                value = newTopic,
                                onValueChange = { newTopic = it },
                                label = { Text("Topic") },
                                singleLine = true,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = newText,
                                onValueChange = { newText = it },
                                label = { Text("Remember…") },
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                val busy = "new" in acting
                                TextButton(
                                    onClick = {
                                        if (forgetArmed) {
                                            forgetArmed = false
                                            onForget()
                                        } else {
                                            forgetArmed = true
                                        }
                                    },
                                    enabled = "forget" !in acting,
                                ) {
                                    Text(if (forgetArmed) "Tap again to forget all" else "Forget all")
                                }
                                TextButton(
                                    onClick = {
                                        onCreate(newTopic, newText) {
                                            newTopic = ""
                                            newText = ""
                                        }
                                    },
                                    enabled = newText.isNotBlank() && !busy,
                                ) {
                                    Text(if (busy) "Saving…" else "Save")
                                }
                            }
                        }
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = onClose) { Text("Close") }
        },
    )
}

@Composable
private fun MemoryRow(
    item: MemoryItem,
    editing: Boolean,
    editText: String,
    editTopic: String,
    onEditText: (String) -> Unit,
    onEditTopic: (String) -> Unit,
    busy: Boolean,
    onStartEdit: () -> Unit,
    onCancelEdit: () -> Unit,
    onSaveEdit: () -> Unit,
    /** Null for confirmed rows: nothing to confirm. */
    onConfirm: (() -> Unit)?,
    onDelete: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth()) {
        if (editing) {
            OutlinedTextField(
                value = editText,
                onValueChange = onEditText,
                label = { Text("Text") },
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = editTopic,
                onValueChange = onEditTopic,
                label = { Text("Topic") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
            )
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.End,
            ) {
                TextButton(onClick = onCancelEdit) { Text("Cancel") }
                TextButton(onClick = onSaveEdit, enabled = !busy && editText.isNotBlank()) {
                    Text(if (busy) "Saving…" else "Save")
                }
            }
        } else {
            Text(
                item.text,
                style = MaterialTheme.typography.bodyMedium,
            )
            val meta = listOf(
                item.topic.ifBlank { null },
                item.source.ifBlank { null },
                relativeAgo(item.createdAt, System.currentTimeMillis()).ifBlank { null },
            ).filterNotNull().joinToString(" · ")
            if (meta.isNotEmpty()) {
                Text(
                    meta,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 2.dp),
                )
            }
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.End,
            ) {
                if (onConfirm != null) {
                    TextButton(onClick = onConfirm, enabled = !busy) {
                        Text(if (busy) "Confirming…" else "Confirm")
                    }
                }
                TextButton(onClick = onStartEdit, enabled = !busy) { Text("Edit") }
                TextButton(onClick = onDelete, enabled = !busy) { Text("Delete") }
            }
        }
    }
}
