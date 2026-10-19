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
    /** Sheet-scoped failure (load/cancel): shown here, never as a chat
     * error with a resend Retry. */
    runsError: String = "",
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
                if (runsError.isNotBlank()) {
                    Text(
                        runsError,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.fillMaxWidth().padding(bottom = 8.dp),
                    )
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
                        modifier = Modifier.heightIn(max = 420.dp),
                        verticalArrangement = Arrangement.spacedBy(12.dp),
                    ) {
                        items(runs, key = { it.id }) { run ->
                            Column(modifier = Modifier.fillMaxWidth()) {
                                // Live label follows the Cancel gate: anything
                                // non-terminal is live (running today,
                                // more states tomorrow), blank means
                                // unparseable. Hoisted to the row scope: the
                                // header, the button and the hint below all
                                // read it.
                                val live = !run.isTerminal() && run.status.isNotBlank()
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    verticalAlignment = Alignment.CenterVertically,
                                ) {
                                    Text(
                                        if (live) "● Running"
                                        else if (run.status.isBlank()) "· unknown"
                                        else run.status,
                                        style = MaterialTheme.typography.labelMedium,
                                        color = if (live) {
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
                                    // Cancel for live runs: `running` today,
                                    // anything non-terminal tomorrow. Blank
                                    // statuses (parse failures) get no button
                                    // — cancelling those 404s.
                                    if (!run.isTerminal() && run.status.isNotBlank()) {
                                        TextButton(onClick = { onCancel(run.id) }) {
                                            Text("Cancel")
                                        }
                                    }
                                }
                                // Settled rows lead with the error when there is
                                // one (failed verdict, cancel/interrupt notes);
                                // live rows show output. Blank errors fall
                                // through to output either way.
                                val body = if (!live && run.error.isNotBlank()) {
                                    run.error
                                } else {
                                    run.output
                                }.trim().replace(WS, " ").take(160)
                                if (body.isNotEmpty()) {
                                    Text(
                                        body,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = if (!live && run.error.isNotBlank()) {
                                            MaterialTheme.colorScheme.error
                                        } else {
                                            MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                        maxLines = 3,
                                        overflow = TextOverflow.Ellipsis,
                                        modifier = Modifier.padding(top = 2.dp),
                                    )
                                } else if (live) {
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
