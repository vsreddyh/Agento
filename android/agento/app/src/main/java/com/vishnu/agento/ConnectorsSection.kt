package com.vishnu.agento

import android.content.Intent
import android.os.SystemClock
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
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicLong
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Connectors section (#440): per-connector connect/disconnect against the
 * existing server verbs — no new API, app UI only.
 *
 * Flow: Connect opens the one-time Google consent URL in a browser (the
 * app never sees code/verifier/token — server-only exchange, sealed
 * tokens in Mongo). The section then polls the list until the row flips
 * to connected — bounded (~2 min, then "Check again" covers slower
 * consents). Disconnect clears tokens server-side; already-fetched
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
    // Synchronous tap guard (#484 review): the composition-state locks
    // below apply on the NEXT frame, so two taps in the same frame both
    // see enabled buttons — double-tap Connect would mint two OAuth
    // attempts and abandon the first wait. CAS serializes mutation taps
    // across rows and actions; reset in finally (no suspension inside,
    // so cancellation still re-arms).
    val mutationArmed = remember { AtomicBoolean(true) }
    // Freshness clock for list applies (#484 review): refresh, poll
    // rounds and disconnect re-lists overlap (poll never takes the
    // single-writer CAS), so last-writer-wins would plant stale rows and
    // banners. Each fetch stamps its monotonic start time; an apply lands
    // only when no fresher fetch has applied since. A counter would order
    // by application, not by fetch — a later fetch applying first must
    // not veto fresher data.
    val freshClock = remember { AtomicLong(0L) }

    // Single-writer/read gates, computed once per composition (#484
    // review): concurrent refresh/disconnect writers race (last writer
    // wins — a slow failure over fresh rows, or a stale list over a just
    // disconnected row), and same-frame double taps bypass composition
    // locks. Passed to the rows; reused below for the retry and the
    // bottom Check again so every gate reads the same values. Connect
    // joins the loading gate too: consent-starting into a list fetch
    // races it the same way a mutation does.
    val connectLocked = pendingConnect != null || busyId != null || loading
    val mutateLocked = busyId != null || loading

    fun path(): String = catalogPath(ChatApi(context))

    fun refresh() {
        // Single-writer entry guard (#484 review): same-frame double taps,
        // resume-vs-tap races and the startup double-fetch must not queue
        // overlapping fetches whose last writer wins with stale data. The
        // CAS runs on the caller thread (tap composition, resume effect),
        // not inside the coroutine — checking inside would serialize
        // nothing. Reset in finally so a finished fetch re-arms
        // (cancellation-safe: no suspension inside).
        if (!mutationArmed.compareAndSet(true, false)) return
        scope.launch {
            try {
                loading = true
                error = ""
                // Freshness anchor: monotonic fetch-start time. Overlapping
                // writers resolve by fetch order, not by who returns last —
                // a counter would order by application and let an older
                // fetch applying last veto fresher data (#484 review).
                val myStart = SystemClock.uptimeMillis()
                ConnectorsApi(context).list(path()).fold(
                    onSuccess = { list ->
                        // Stale successes must not overwrite fresher rows.
                        // Resolution below stays unconditional — see the
                        // poll branch for why.
                        if (myStart >= freshClock.getAndUpdate { cur ->
                                if (myStart >= cur) myStart else cur
                            }
                        ) {
                            connectors = list
                            // Cleared on success, not just at start (#484 review):
                            // a failed refresh followed by a good one (manual retry
                            // landing just as the poll round succeeds, or vice
                            // versa) must not leave its banner over fresh rows.
                            error = ""
                        }
                        // A wait resolves on connected ONLY (#484 review):
                        // POST /connect never changes status, so the row a
                        // Reconnect tap starts from is already expired — an
                        // expired check would resolve on round 1 with no
                        // toast and never auto-resolve. An expiry appearing
                        // mid-wait from a disconnected start is impossible
                        // (no tokens to age out), so connected-only loses
                        // nothing. The expired row keeps rendering its
                        // reconnect hint meanwhile.
                        val pending = pendingConnect
                        if (pending != null &&
                            list.any { it.id == pending && it.isConnected() }
                        ) {
                            pendingConnect = null
                            toasts.toast("Connected.", kind = ToastKind.Success)
                        }
                    },
                    onFailure = { e ->
                        // Only when nothing fresher applied meanwhile: a
                        // success after this fetch started already proved
                        // the rows (#484 review).
                        if (myStart >= freshClock.get()) {
                            error = e.message ?: e.javaClass.simpleName
                        }
                    },
                )
                loading = false
            } finally {
                mutationArmed.set(true)
            }
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
        // Wait-scoped, not remembered: each consent wait gets a fresh
        // verdict. Error only when NO round succeeded — one transient
        // blip in 39 good rounds is not a dead server, and one good round
        // in 39 failures is not a healthy one (#484 review).
        var anyOk = false
        var lastError = ""
        repeat(40) {
            delay(3_000L)
            if (pendingConnect != pending) return@LaunchedEffect
            // Freshness anchor, like refresh: overlapping writers resolve
            // by fetch order (#484 review).
            val roundStart = SystemClock.uptimeMillis()
            ConnectorsApi(context).list(path()).fold(
                onSuccess = { list ->
                    // Wait key re-checked BEFORE applying (#484 review): a
                    // disconnect landing mid-IO must not have its mapping
                    // overwritten by this stale round — and with the wait
                    // cancelled there is no later round to heal it. Refresh
                    // resolutions and newer waits own their own applies.
                    if (pendingConnect != pending) return@LaunchedEffect
                    // Data apply is freshness-guarded, resolution is not:
                    // a skipped round must not overwrite fresher rows, but
                    // a flip this list proves still resolves the wait — a
                    // flip predates any newer fetch, so newer data either
                    // shows it too (already resolved, resolver no-ops) or
                    // the wait is gone (disconnect cleared it).
                    if (roundStart >= freshClock.getAndUpdate { cur ->
                            if (roundStart >= cur) roundStart else cur
                        }
                    ) {
                        connectors = list
                        // Same stale-banner rule as refresh() above: a good
                        // poll round heals a failed manual refresh (#484
                        // review).
                        error = ""
                    }
                    anyOk = true
                    lastError = ""
                    // Connected-only resolution, like refresh() above (#484
                    // review): a pre-existing expired row must not end the
                    // wait on round 1.
                    if (list.any { it.id == pending && it.isConnected() }) {
                        pendingConnect = null
                        toasts.toast("Connected.", kind = ToastKind.Success)
                    }
                },
                onFailure = { e ->
                    lastError = e.message ?: e.javaClass.simpleName
                },
            )
        }
        // Wait ran out with the row still unchanged: stop polling, keep
        // the manual check. A wait that never reached the server is a
        // dead server, not a slow consent; anything else is still just
        // waiting on the browser.
        if (pendingConnect == pending) {
            pendingConnect = null
            if (anyOk) {
                toasts.toast("Still waiting — tap Check again after finishing in the browser.")
            } else {
                toasts.toast(
                    lastError.ifEmpty { "Couldn't reach the server." },
                    kind = ToastKind.Error,
                )
            }
        }
    }

    SectionCard(
        title = "Connected accounts",
        subtitle = "Gmail and Calendar for chat lookups. Consent happens in the browser — never paste a password or token in chat.",
    ) {
        if (loading) {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
        }
        // A failed refresh says so even over stale rows (#484 review): a
        // dead server must never read as a confirmed list.
        if (error.isNotEmpty()) {
            ErrorCard(
                raw = error,
                // Gated while loading or mutating (#484 review): two rapid
                // taps would launch concurrent refreshes whose last writer
                // wins — a slow failure landing after a fresh success
                // banners over good rows. No retry slot while busy either.
                onRetry = if (!mutateLocked) {
                    { refreshTick++ }
                } else {
                    null
                },
            )
        }
        connectors.forEach { c ->
            ConnectorRow(
                info = c,
                busy = busyId == c.id,
                pending = pendingConnect == c.id,
                // One wait at a time (#484 review): a second Connect
                // while any consent is pending would overwrite
                // pendingConnect and abandon the first wait with no
                // toast and no cleanup. busyId joins the lock so a
                // second tap during the first fetch cannot do it either.
                connectLocked = connectLocked,
                // Disconnects serialize the same way: a second tap while
                // one is in flight would overwrite busyId, and the first
                // completion would re-enable the second button mid-request
                // (#484 review). Loading joins this lock: a refresh in
                // flight when a disconnect lands would return its
                // pre-disconnect list over the new mapping — and a
                // Check-again tap during either would queue a second
                // writer behind it. Pending consent does not lock
                // Disconnect — rows are independent, only in-flight work
                // serializes.
                mutateLocked = mutateLocked,
                onConnect = {
                    // Synchronous tap guard (see mutationArmed above):
                    // same-frame double taps both see enabled buttons.
                    if (mutationArmed.compareAndSet(true, false)) {
                        busyId = c.id
                        scope.launch {
                            try {
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
                                            return@launch
                                        }
                                        pendingConnect = c.id
                                    },
                                    onFailure = { e ->
                                        val msg = e.message.orEmpty()
                                        // Unconfigured server (501) must read as
                                        // "not available", never as an invitation
                                        // to paste a secret somewhere else.
                                        // Matched on the built status code, not
                                        // a substring that could sit anywhere
                                        // in the body (#484 review).
                                        toasts.toast(
                                            if (isConnectNotConfigured(msg)) {
                                                "Connect isn't set up on this server yet."
                                            } else {
                                                msg.ifEmpty { e.javaClass.simpleName }
                                            },
                                            kind = ToastKind.Error,
                                        )
                                    },
                                )
                            } finally {
                                busyId = null
                                mutationArmed.set(true)
                            }
                        }
                    }
                },
                onDisconnect = {
                    if (mutationArmed.compareAndSet(true, false)) {
                        busyId = c.id
                        scope.launch {
                            try {
                                ConnectorsApi(context).disconnect(path(), c.id).fold(
                                    onSuccess = {
                                        // A disconnect scrubs the server-side pending
                                        // states for this id: a consent wait on it
                                        // can never resolve, so stop polling now
                                        // instead of running 40 rounds into a "Still
                                        // waiting" toast for the impossible (#484
                                        // review).
                                        if (pendingConnect == c.id) pendingConnect = null
                                        // Full re-list inline, not the row
                                        // shortcut and not a tick bump (#484
                                        // review): the shortcut cannot prove
                                        // the OTHER rows fresh after an
                                        // outage (banner cleared over stale
                                        // rows), and a tick bump could
                                        // CAS-fail on our own held guard and
                                        // heal nothing. Same error slot as
                                        // refresh, so success and failure
                                        // cannot disagree about staleness.
                                        // Freshness-guarded like every other
                                        // apply.
                                        val regen = SystemClock.uptimeMillis()
                                        ConnectorsApi(context).list(path()).fold(
                                            onSuccess = { list ->
                                                if (regen >= freshClock.getAndUpdate { cur ->
                                                        if (regen >= cur) regen else cur
                                                    }
                                                ) {
                                                    connectors = list
                                                    error = ""
                                                }
                                                toasts.toast(
                                                    "Disconnected — future access stopped.",
                                                    kind = ToastKind.Success,
                                                )
                                            },
                                            onFailure = { e ->
                                                if (regen >= freshClock.get()) {
                                                    error = e.message ?: e.javaClass.simpleName
                                                }
                                                toasts.toast(
                                                    "Disconnected — but the list couldn't refresh. Tap Check again.",
                                                    kind = ToastKind.Error,
                                                )
                                            },
                                        )
                                    },
                                    onFailure = { e ->
                                        toasts.toast(
                                            e.message ?: e.javaClass.simpleName,
                                            kind = ToastKind.Error,
                                        )
                                    },
                                )
                            } finally {
                                busyId = null
                                mutationArmed.set(true)
                            }
                        }
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
            // Same gate as the retry above: one refresh writer at a time.
            enabled = !mutateLocked,
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
    connectLocked: Boolean,
    mutateLocked: Boolean,
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
                OutlinedButton(onClick = onDisconnect, enabled = !busy && !mutateLocked, modifier = Modifier.weight(1f)) {
                    Text(if (busy) "Working…" else "Disconnect")
                }
            } else {
                Button(
                    onClick = onConnect,
                    enabled = !busy && !connectLocked,
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
                // Expired rows keep a Disconnect next to Reconnect (#484
                // review): the server revokes expired grants without a
                // fresh consent, so revoking must not require re-granting
                // first. Same mutation gate as the connected path.
                if (info.isExpired()) {
                    OutlinedButton(
                        onClick = onDisconnect,
                        enabled = !busy && !mutateLocked,
                        modifier = Modifier.weight(1f),
                    ) {
                        Text(if (busy) "Working…" else "Disconnect")
                    }
                }
                if (pending) {
                    OutlinedButton(
                        onClick = onCheckAgain,
                        // Same single-writer gate as the retry above.
                        enabled = !mutateLocked,
                        modifier = Modifier.weight(1f),
                    ) {
                        Text("Check again")
                    }
                }
            }
        }
    }
}
