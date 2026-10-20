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
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay

/** A clock that ticks every 30s while the sheet is open, so "2m waiting"
 * advances instead of freezing at the composition instant. */
@Composable
private fun rememberNowTick(): Long {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(30_000)
            now = System.currentTimeMillis()
        }
    }
    return now
}

/** How long an approval has waited: "just now" then "2m waiting". */
internal fun waitingLabel(createdAt: Long, now: Long): String {
    val age = relativeAgo(createdAt, now)
    return if (age.isEmpty() || age == "just now") age else "${age.removeSuffix(" ago")} waiting"
}

/** Millis instant as "just now / Nm ago / Nh ago / Nd ago", "" when unknown.
 * Future instants clamp to "just now": clock skew is not a time machine. */
internal fun relativeAgo(at: Long, now: Long): String {
    if (at <= 0) return ""
    val s = (now - at) / 1000
    return when {
        s < 60 -> "just now"
        s < 3600 -> "${s / 60}m ago"
        s < 86400 -> "${s / 3600}h ago"
        else -> "${s / 86400}d ago"
    }
}

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
    degraded: Boolean = false,
    /** Ids with a decision in flight: their buttons disable so a double tap
     * cannot fire two decisions. */
    deciding: Set<String> = emptySet(),
    onApprove: (String) -> Unit,
    onDeny: (String) -> Unit,
    onRefresh: () -> Unit,
    onClose: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Approvals") },
        text = {
            val now = rememberNowTick()
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
                if (degraded) {
                    Text(
                        "Approvals unavailable: the server cannot reach held calls right now — decisions would not land.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.tertiary,
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
                        // Bounded: an unbounded list inside a dialog can
                        // overflow or refuse to scroll on small screens.
                        modifier = Modifier.heightIn(max = 420.dp),
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
                                    // Age, oldest-first triage: a gate held since
                                    // the turn started is the one to answer.
                                    val waiting = waitingLabel(approval.createdAt, now)
                                    if (waiting.isNotEmpty()) {
                                        Text(
                                            waiting,
                                            style = MaterialTheme.typography.labelSmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        )
                                    }
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
                                    val busy = approval.id in deciding
                                    TextButton(
                                        onClick = { onDeny(approval.id) },
                                        enabled = !busy,
                                    ) {
                                        Text("Deny")
                                    }
                                    TextButton(
                                        onClick = { onApprove(approval.id) },
                                        enabled = !busy,
                                    ) {
                                        Text(if (busy) "Deciding…" else "Approve")
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
                                    val whenText = relativeAgo(row.at, now)
                                    Text(
                                        "${row.server} · ${row.tool} · ${row.disposition.ifBlank { "?" }}" +
                                            (if (whenText.isNotEmpty()) " · $whenText" else ""),
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
