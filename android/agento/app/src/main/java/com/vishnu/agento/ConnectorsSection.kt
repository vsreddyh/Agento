package com.vishnu.agento

import android.content.Intent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.Button
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.net.toUri
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Connectors section (#440): per-connector connect/disconnect against the
 * existing server verbs — no new API, app UI only.
 *
 * Flow: Connect opens the one-time Google consent URL in a browser (the
 * app never sees code/verifier/token — server-only exchange, sealed
 * tokens in Mongo). The section then polls the list until the row flips
 * to connected. Disconnect clears tokens server-side; already-fetched
 * chat history is NOT wiped (stops the tap, not history — ADR-0009).
 * Expired rows surface the wire `reconnect` hint as a reconnect prompt.
 * Consent happens in the browser: never paste a password or token in chat.
 */
@Composable
internal fun ConnectorsSection(toasts: ToastCenter) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var connectors by remember { mutableStateOf<List<ConnectorInfo>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    var refreshTick by remember { mutableIntStateOf(0) }
    var resumeTick by remember { mutableIntStateOf(0) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { resumeTick++ }
    // Connector id with a consent page open in the browser: poll until it
    // flips or the wait runs out. Returning from the browser fires
    // ON_RESUME, which re-lists immediately (same tick below).
    var pendingConnect by remember { mutableStateOf<String?>(null) }
    var busyId by remember { mutableStateOf<String?>(null) }

    fun path(): String = catalogPath(ChatApi(context))

    fun refresh() {
        scope.launch {
            loading = true
            error = ""
            ConnectorsApi(context).list(path()).fold(
                onSuccess = { list ->
                    connectors = list
                    // A polled row that flipped resolves the wait (#440):
                    // the pending id is connected (or failed visibly via
                    // an expired row carrying the reconnect hint).
                    val pending = pendingConnect
                    if (pending != null &&
                        list.any { it.id == pending && (it.isConnected() || it.isExpired()) }
                    ) {
                        pendingConnect = null
                        if (list.any { it.id == pending && it.isConnected() }) {
                            toasts.toast("Connected.", kind = ToastKind.Success)
                        }
                    }
                },
                onFailure = { e ->
                    error = e.message ?: e.javaClass.simpleName
                },
            )
            loading = false
        }
    }

    LaunchedEffect(refreshTick, resumeTick) { refresh() }

    // Bounded poll while a consent page is outstanding: the callback lands
    // server-side, so the app learns about it by re-listing. Forty rounds
    // at 3s ≈ 2 minutes, past the point anyone still stares at this
    // screen — the "Check again" button below covers slower consents.
    val pending = pendingConnect
    LaunchedEffect(pending) {
        if (pending == null) return@LaunchedEffect
        repeat(40) {
            delay(3_000L)
            if (pendingConnect != pending) return@LaunchedEffect
            ConnectorsApi(context).list(path()).fold(
                onSuccess = { list ->
                    connectors = list
                    if (list.any { it.id == pending && (it.isConnected() || it.isExpired()) }) {
                        pendingConnect = null
                        if (list.any { it.id == pending && it.isConnected() }) {
                            toasts.toast("Connected.", kind = ToastKind.Success)
                        }
                    }
                },
                onFailure = { },
            )
        }
        // Wait ran out with the row still unchanged: stop polling, keep
        // the manual check. Not an error — the consent page may still be
        // open in the browser.
        if (pendingConnect == pending) {
            pendingConnect = null
            toasts.toast("Still waiting — tap Check again after finishing in the browser.")
        }
    }

    SectionCard(
        title = "Connected accounts",
        subtitle = "Gmail and Calendar for chat lookups. Consent happens in the browser — never paste a password or token in chat.",
    ) {
        if (loading && connectors.isEmpty()) {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
        }
        if (error.isNotEmpty() && connectors.isEmpty()) {
            ErrorCard(raw = error, onRetry = { refreshTick++ })
        }
        connectors.forEach { c ->
            ConnectorRow(
                info = c,
                busy = busyId == c.id,
                pending = pendingConnect == c.id,
                onConnect = {
                    busyId = c.id
                    scope.launch {
                        ConnectorsApi(context).connect(path(), c.id).fold(
                            onSuccess = { url ->
                                runCatching {
                                    context.startActivity(
                                        Intent(Intent.ACTION_VIEW, url.toUri())
                                            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                                    )
                                }.onFailure {
                                    toasts.toast(
                                        "Couldn't open the browser.",
                                        kind = ToastKind.Error,
                                    )
                                    busyId = null
                                    return@launch
                                }
                                busyId = null
                                pendingConnect = c.id
                            },
                            onFailure = { e ->
                                busyId = null
                                val msg = e.message.orEmpty()
                                // Unconfigured server (501) must read as
                                // "not available", never as an invitation
                                // to paste a secret somewhere else.
                                toasts.toast(
                                    if (msg.contains("501")) {
                                        "Connect isn't set up on this server yet."
                                    } else {
                                        msg.ifEmpty { e.javaClass.simpleName }
                                    },
                                    kind = ToastKind.Error,
                                )
                            },
                        )
                    }
                },
                onDisconnect = {
                    busyId = c.id
                    scope.launch {
                        ConnectorsApi(context).disconnect(path(), c.id).fold(
                            onSuccess = { row ->
                                busyId = null
                                if (row != null) {
                                    connectors = connectors.map {
                                        if (it.id == row.id) row else it
                                    }
                                } else {
                                    refreshTick++
                                }
                                toasts.toast(
                                    "Disconnected — future access stopped.",
                                    kind = ToastKind.Success,
                                )
                            },
                            onFailure = { e ->
                                busyId = null
                                toasts.toast(
                                    e.message ?: e.javaClass.simpleName,
                                    kind = ToastKind.Error,
                                )
                            },
                        )
                    }
                },
                onCheckAgain = { refreshTick++ },
            )
        }
        if (!loading && error.isEmpty() && connectors.isEmpty()) {
            HintLine("No connectors listed — check the server connection.")
        }
        OutlinedButton(
            onClick = { refreshTick++ },
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text("Check again")
        }
        HintLine("Disconnect stops future access; already-fetched chat history stays.")
    }
}


/** One connector row: status, explicit scopes, connect/disconnect. */
@Composable
private fun ConnectorRow(
    info: ConnectorInfo,
    busy: Boolean,
    pending: Boolean,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onCheckAgain: () -> Unit,
) {
    SectionCard(
        title = info.displayName.ifEmpty { info.id },
        subtitle = when {
            info.isConnected() -> "Connected"
            info.isExpired() -> "Session expired"
            info.status.isNotEmpty() -> info.status.replaceFirstChar { it.uppercase() }
            else -> "Not connected"
        },
    ) {
        // Explicit scopes at connect time (#440): the row names what a
        // tap grants before the consent page opens.
        if (info.scopes.isNotEmpty()) {
            Text(
                "Allows: ${info.scopes.joinToString(", ")}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        if (info.lastError.isNotEmpty()) {
            ErrorCard(raw = info.lastError)
        }
        // Expired rows surface the wire hint as the reconnect prompt
        // (#440): the server text already says where, never asks for a
        // secret, and stays the single wording (no paraphrase to drift).
        if (info.isExpired() && info.reconnect.isNotEmpty()) {
            HintLine(info.reconnect)
        }
        if (pending) {
            HintLine("Waiting for the browser consent — finish there, then Check again.")
        }
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            if (info.isConnected()) {
                OutlinedButton(onClick = onDisconnect, enabled = !busy, modifier = Modifier.weight(1f)) {
                    Text(if (busy) "Working…" else "Disconnect")
                }
            } else {
                Button(
                    onClick = onConnect,
                    enabled = !busy && !pending,
                    modifier = Modifier.weight(1f),
                ) {
                    Text(
                        when {
                            busy -> "Starting…"
                            pending -> "Waiting…"
                            info.isExpired() -> "Reconnect"
                            else -> "Connect"
                        },
                    )
                }
                if (pending) {
                    OutlinedButton(onClick = onCheckAgain, modifier = Modifier.weight(1f)) {
                        Text("Check again")
                    }
                }
            }
        }
    }
}
