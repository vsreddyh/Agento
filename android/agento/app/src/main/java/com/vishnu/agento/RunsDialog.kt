package com.vishnu.agento

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

private val WS = Regex("\\s+")

/**
 * The thread's server-side turns (#311): status plus last output per run,
 * in-flight vs finished at a glance, with cancel for the running ones.
 * Cancelling keeps the partial output — the turn's stream then ends through
 * the normal cancelled path and the reply lands interrupted, not lost.
 */
@Composable
internal fun RunsDialog(
    runs: List<AgentRun>,
    loading: Boolean,
    /** Id of the run blocking this tab (429/error stash): highlighted so the
     * user can spot the row to watch or cancel. */
    highlightId: String = "",
    onRefresh: () -> Unit,
    onCancel: (String) -> Unit,
    onClose: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Runs") },
        text = {
            Column(modifier = Modifier.fillMaxWidth()) {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.End,
                ) {
                    TextButton(onClick = onRefresh) { Text("Refresh") }
                }
                when {
                    loading && runs.isEmpty() -> Text(
                        "Loading…",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    runs.isEmpty() -> Text(
                        "No runs for this conversation yet.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    else -> LazyColumn(
                        verticalArrangement = Arrangement.spacedBy(12.dp),
                    ) {
                        items(runs, key = { it.id }) { run ->
                            Column(modifier = Modifier.fillMaxWidth()) {
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    verticalAlignment = Alignment.CenterVertically,
                                ) {
                                    Text(
                                        if (run.isRunning()) "● Running"
                                        else if (run.status.isBlank()) "· unknown"
                                        else run.status,
                                        style = MaterialTheme.typography.labelMedium,
                                        color = if (run.isRunning()) {
                                            MaterialTheme.colorScheme.primary
                                        } else {
                                            MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                        modifier = Modifier.weight(1f),
                                    )
                                    Text(
                                        run.id.take(8),
                                        style = MaterialTheme.typography.labelSmall,
                                        color = if (run.id == highlightId && highlightId.isNotBlank()) {
                                            MaterialTheme.colorScheme.primary
                                        } else {
                                            MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                    )
                                    if (!run.isTerminal()) {
                                        TextButton(onClick = { onCancel(run.id) }) {
                                            Text("Cancel")
                                        }
                                    }
                                }
                                // A live run with nothing yet is still starting,
                                // not broken: say so instead of a bare header.
                                val body = if (run.status == "failed" && run.error.isNotBlank()) {
                                    run.error
                                } else {
                                    run.output
                                }.trim().replace(WS, " ").take(160)
                                if (body.isNotEmpty()) {
                                    Text(
                                        body,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = if (run.status == "failed" && run.error.isNotBlank()) {
                                            MaterialTheme.colorScheme.error
                                        } else {
                                            MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                        maxLines = 3,
                                        overflow = TextOverflow.Ellipsis,
                                        modifier = Modifier.padding(top = 2.dp),
                                    )
                                } else if (run.isRunning()) {
                                    Text(
                                        "· waiting for output…",
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        modifier = Modifier.padding(top = 2.dp),
                                    )
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
