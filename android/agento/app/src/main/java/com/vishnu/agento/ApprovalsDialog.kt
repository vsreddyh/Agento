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

/**
 * Held tool calls and the audit trail (#313).
 *
 * Each pending card names the server + tool with its redacted arguments:
 * approve runs it, deny fails it back to the turn as a tool error (the turn
 * continues either way). Deciding is a verb on an id — never a pasted
 * secret. Below, the recent audit rows show what ran and how it was
 * decided.
 */
@Composable
internal fun ApprovalsDialog(
    approvals: List<GateApproval>,
    auditRows: List<AuditRow>,
    loading: Boolean,
    error: String,
    onApprove: (String) -> Unit,
    onDeny: (String) -> Unit,
    onRefresh: () -> Unit,
    onClose: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Approvals") },
        text = {
            Column(modifier = Modifier.fillMaxWidth()) {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.End,
                ) {
                    TextButton(onClick = onRefresh) { Text("Refresh") }
                }
                if (error.isNotBlank()) {
                    Text(
                        error,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.fillMaxWidth().padding(bottom = 8.dp),
                    )
                }
                if (loading && approvals.isEmpty() && auditRows.isEmpty()) {
                    Text(
                        "Loading…",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    LazyColumn(
                        verticalArrangement = Arrangement.spacedBy(12.dp),
                    ) {
                        if (approvals.isEmpty()) {
                            item {
                                Text(
                                    "No pending approvals.",
                                    style = MaterialTheme.typography.bodyMedium,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                        items(approvals, key = { it.id }) { approval ->
                            Column(modifier = Modifier.fillMaxWidth()) {
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    verticalAlignment = Alignment.CenterVertically,
                                ) {
                                    Text(
                                        "● ${approval.tool}",
                                        style = MaterialTheme.typography.labelMedium,
                                        color = MaterialTheme.colorScheme.primary,
                                        modifier = Modifier.weight(1f),
                                    )
                                }
                                if (approval.server.isNotBlank()) {
                                    Text(
                                        approval.server,
                                        style = MaterialTheme.typography.labelSmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                                val args = approval.argsSummary()
                                if (args.isNotEmpty()) {
                                    Text(
                                        args,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        maxLines = 3,
                                        overflow = TextOverflow.Ellipsis,
                                        modifier = Modifier.padding(top = 2.dp),
                                    )
                                }
                                Row(
                                    modifier = Modifier.fillMaxWidth(),
                                    horizontalArrangement = Arrangement.End,
                                ) {
                                    TextButton(onClick = { onDeny(approval.id) }) {
                                        Text("Deny")
                                    }
                                    TextButton(onClick = { onApprove(approval.id) }) {
                                        Text("Approve")
                                    }
                                }
                            }
                        }
                        if (auditRows.isNotEmpty()) {
                            item {
                                Text(
                                    "Recent activity",
                                    style = MaterialTheme.typography.titleSmall,
                                    modifier = Modifier.padding(top = 4.dp),
                                )
                            }
                            items(auditRows.take(20), key = { it.id }) { row ->
                                Column(modifier = Modifier.fillMaxWidth()) {
                                    Text(
                                        "${row.tool} · ${row.disposition.ifBlank { "?" }}",
                                        style = MaterialTheme.typography.labelMedium,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                    if (row.error.isNotBlank()) {
                                        Text(
                                            row.error.take(160),
                                            style = MaterialTheme.typography.bodySmall,
                                            color = MaterialTheme.colorScheme.error,
                                            maxLines = 2,
                                            overflow = TextOverflow.Ellipsis,
                                        )
                                    }
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
