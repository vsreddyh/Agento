package com.vishnu.agento

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
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
 * Review surface for one taught skill (#314).
 *
 * Drafts edit freely; Test runs the procedure once and shows the
 * transcript; Activate needs that test (the server enforces it);
 * Promote schedules a cron job carrying a copy of the reviewed text.
 * Every write disables its buttons while in flight so a double tap
 * cannot fire two writes.
 */
@Composable
internal fun TeachDialog(
    skill: TaughtSkill,
    testOutput: String,
    acting: Boolean,
    error: String,
    /** Successful saves from the screen: each increment collapses an open
     * editor, so a saved edit never lingers over fresh rows. */
    editSaves: Int = 0,
    onSaveEdit: (String, String, String) -> Unit,
    onTest: (String) -> Unit,
    onActivate: () -> Unit,
    onDisable: () -> Unit,
    onPromote: (String, String) -> Unit,
    onDelete: () -> Unit,
    onClose: () -> Unit,
) {
    var editing by remember(skill.id) { mutableStateOf(false) }
    var name by remember(skill.id) { mutableStateOf(skill.name) }
    var description by remember(skill.id) { mutableStateOf(skill.description) }
    var body by remember(skill.id) { mutableStateOf(skill.body) }
    var testInput by remember(skill.id) { mutableStateOf("") }
    var schedule by remember(skill.id) { mutableStateOf("") }
    var promoteName by remember(skill.id) { mutableStateOf("") }
    var deleteArmed by remember(skill.id) { mutableStateOf(false) }
    LaunchedEffect(editSaves) {
        if (editSaves > 0) editing = false
    }
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text(skill.name.ifBlank { "Skill" }) },
        text = {
            Column(modifier = Modifier.fillMaxWidth()) {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text(
                        skill.status.ifBlank { "?" },
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.primary,
                        modifier = Modifier.weight(1f),
                    )
                    if (!skill.isTested()) {
                        Text(
                            "untested",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                if (error.isNotBlank()) {
                    Text(
                        error,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.fillMaxWidth().padding(vertical = 8.dp),
                    )
                }
                LazyColumn(
                    // Bounded: an unbounded list inside a dialog can
                    // overflow or refuse to scroll on small screens.
                    modifier = Modifier.heightIn(max = 420.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    if (editing) {
                        item {
                            OutlinedTextField(
                                value = name,
                                onValueChange = { name = it },
                                label = { Text("Name (lowercase, hyphens)") },
                                singleLine = true,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = description,
                                onValueChange = { description = it },
                                label = { Text("Description") },
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = body,
                                onValueChange = { body = it },
                                label = { Text("Procedure") },
                                minLines = 4,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                            ) {
                                TextButton(onClick = { editing = false }) { Text("Cancel") }
                                TextButton(
                                    // Trimmed like every other writer: "   "
                                    // passes isNotBlank but sends "".
                                    onClick = { onSaveEdit(name.trim(), description.trim(), body.trim()) },
                                    enabled = !acting && name.trim().isNotBlank() && body.trim().isNotBlank(),
                                ) {
                                    Text(if (acting) "Saving…" else "Save")
                                }
                            }
                        }
                    } else {
                        if (skill.description.isNotBlank()) {
                            item {
                                Text(
                                    skill.description,
                                    style = MaterialTheme.typography.bodyMedium,
                                )
                            }
                        }
                        item {
                            Text(
                                skill.body,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        if (skill.testText.isNotBlank()) {
                            item {
                                Text(
                                    "Last test: ${skill.testText.take(300)}",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.tertiary,
                                    maxLines = 4,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                            ) {
                                TextButton(
                                    onClick = { editing = true },
                                    enabled = !acting,
                                ) {
                                    Text("Edit")
                                }
                                if (skill.isActive()) {
                                    TextButton(onClick = onDisable, enabled = !acting) {
                                        Text("Disable")
                                    }
                                } else {
                                    // Drafts and disabled rows alike: the
                                    // server refuses untested activations
                                    // with a message, so both offer it.
                                    TextButton(onClick = onActivate, enabled = !acting) {
                                        Text(if (acting) "Working…" else "Activate")
                                    }
                                }
                            }
                        }
                        item {
                            Text(
                                "Test run",
                                style = MaterialTheme.typography.titleSmall,
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = testInput,
                                onValueChange = { testInput = it },
                                label = { Text("Test input (optional)") },
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                            ) {
                                TextButton(
                                    onClick = { onTest(testInput) },
                                    enabled = !acting,
                                ) {
                                    Text(if (acting) "Running…" else "Run test")
                                }
                            }
                        }
                        if (testOutput.isNotBlank()) {
                            item {
                                Text(
                                    testOutput.take(600),
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 8,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                        if (skill.isActive() && skill.isTested()) {
                            item {
                                Text(
                                    "Schedule routine",
                                    style = MaterialTheme.typography.titleSmall,
                                )
                            }
                            item {
                                OutlinedTextField(
                                    value = schedule,
                                    onValueChange = { schedule = it },
                                    label = { Text("Schedule (cron, e.g. 0 9 * * 1)") },
                                    singleLine = true,
                                    modifier = Modifier.fillMaxWidth(),
                                )
                            }
                            item {
                                OutlinedTextField(
                                    value = promoteName,
                                    onValueChange = { promoteName = it },
                                    label = { Text("Routine name (optional)") },
                                    singleLine = true,
                                    modifier = Modifier.fillMaxWidth(),
                                )
                            }
                            item {
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    horizontalArrangement = Arrangement.End,
                                ) {
                                    TextButton(
                                        onClick = { onPromote(schedule.trim(), promoteName.trim()) },
                                        enabled = !acting && schedule.trim().isNotBlank(),
                                    ) {
                                        Text(if (acting) "Scheduling…" else "Schedule")
                                    }
                                }
                            }
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                            ) {
                                TextButton(
                                    onClick = {
                                        if (deleteArmed) {
                                            deleteArmed = false
                                            onDelete()
                                        } else {
                                            deleteArmed = true
                                        }
                                    },
                                    enabled = !acting,
                                ) {
                                    Text(if (deleteArmed) "Tap again to delete" else "Delete")
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
