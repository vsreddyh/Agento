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
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
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

/** Poll rounds per consent wait (3s apart ≈ 2 minutes total). */
private const val POLL_ROUNDS = 40
/** Delay between consent-wait poll rounds. */
private const val POLL_ROUND_MS = 3_000L
/** Total poll budget per wait, across rotations (#484 review). */
private const val POLL_BUDGET_MS = POLL_ROUNDS * POLL_ROUND_MS

/**
 * Connectors section (#440): per-connector connect/disconnect against the
 * existing server verbs — no new API, app UI only.
 *
 * Flow: Connect opens the one-time Google consent URL in a browser (the
 * app never sees code/verifier/token — server-only exchange, sealed
 * tokens in Mongo). The section then polls the list until the row flips
 * to connected — bounded (~2 min of wall clock, re-checked every round
 * so slow round-trips cannot stretch past the attempt TTL, then
 * "Check again" covers slower consents). Disconnect clears tokens
 * server-side; already-fetched
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
    // Saveable across rotation (#484 review): a rotate during the ~2min
    // poll must resume the wait, not silently drop it. busyId deliberately
    // stays plain remember — its coroutine dies with the scope on
    // rotation, so restoring it would wedge buttons on Working… forever.
    var pendingConnect by rememberSaveable { mutableStateOf<String?>(null) }
    // Dead-server verdict, saveable like the wait it describes (#484
    // review): the budget survives rotation, so the verdict must too —
    // otherwise 30 healthy rounds forgotten at rotate turns 10 blips
    // into a dead-server Error (or vice versa). Reset wherever a wait
    // starts, alongside pendingConnect/pendingSince.
    var pollReached by rememberSaveable { mutableStateOf(false) }
    var pollLastError by rememberSaveable { mutableStateOf("") }
    // Wait start (uptime ms) for the poll budget below: rotation restarts
    // the effect, so without this every rotate would grant a fresh 40
    // rounds and repeated rotates could poll past the attempt TTL.
    var pendingSince by rememberSaveable { mutableLongStateOf(0L) }
    var busyId by remember { mutableStateOf<String?>(null) }
    // Synchronous tap guard (#484 review): the composition-state locks
    // below apply on the NEXT frame, so two taps in the same frame both
    // see enabled buttons — double-tap Connect would mint two OAuth
    // attempts and abandon the first wait. CAS serializes mutation taps
    // across rows and actions; reset in finally (no suspension inside,
    // so cancellation still re-arms).
    val mutationArmed = remember { AtomicBoolean(true) }
    // Fetch sequence for list applies (#484 review): refresh, poll
    // rounds and disconnect re-lists overlap (poll never takes the
    // single-writer CAS), so last-writer-wins would plant stale rows and
    // banners. Strictly increasing — ms timestamps collided on fast paths
    // — ordering by fetch START, not by return. Server-side reordering
    // past that (a later-started fetch observing older server state) is
    // unknowable client-side: the poll keeps looping while its wait is
    // open, so a vetoed resolution heals within one round, and one-shot
    // refreshes converge on the next trigger.
    val fetchSeq = remember { AtomicLong(0L) }
    // Highest started sequence whose outcome applied.
    val appliedSeq = remember { AtomicLong(0L) }
    /** Stamps a fetch start; returns the stamp to gate its outcome on. */
    fun stampFetch(): Long = fetchSeq.incrementAndGet()
    /**
     * Claims this fetch's outcome: true when no newer fetch has applied
     * since it started (and records it because it just did). Rows,
     * banners and view-tied toasts apply only on true.
     */
    fun claimFresh(start: Long): Boolean =
        start >= appliedSeq.getAndUpdate { cur -> if (start > cur) start else cur }

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
                // Freshness anchor: monotonic fetch-start time stamped up
                // front, so overlapping writers resolve by fetch order, not
                // by who returns last.
                val myStart = stampFetch()
                ConnectorsApi(context).list(path()).fold(
                    onSuccess = { list ->
                        // Rows, banner and wait resolution share one
                        // freshness gate (#484 review): a vetoed payload
                        // proves nothing — resolving the wait off it would
                        // toast over contradictory rows with no later round
                        // to heal (unlike the poll, a refresh is one-shot).
                        // A vetoed flip heals via the still-open wait's
                        // next poll round instead.
                        if (claimFresh(myStart)) {
                            connectors = list
                            // Cleared on success, not just at start (#484 review):
                            // a failed refresh followed by a good one (manual retry
                            // landing just as the poll round succeeds, or vice
                            // versa) must not leave its banner over fresh rows.
                            error = ""
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
                        }
                    },
                    onFailure = { e ->
                        // Only when nothing fresher applied meanwhile: a
                        // success after this fetch started already proved
                        // the rows (#484 review).
                        if (claimFresh(myStart)) {
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
    // at 3s ≈ 2 minutes total across rotations (see pendingSince), past
    // which nobody still stares at this screen — the "Check again" button
    // below covers slower consents.
    val pending = pendingConnect
    LaunchedEffect(pending) {
        if (pending == null) return@LaunchedEffect
        // Remaining budget, not a fresh 40: the effect restarts on
        // rotation, and a fresh budget per restart would poll past the
        // attempt TTL on repeated rotates (#484 review).
        val elapsed = if (pendingSince == 0L) {
            0L
        } else {
            (SystemClock.uptimeMillis() - pendingSince).coerceAtLeast(0L)
        }
        val rounds = ((POLL_BUDGET_MS - elapsed) / POLL_ROUND_MS).toInt().coerceIn(0, POLL_ROUNDS)
        repeat(rounds) {
            delay(POLL_ROUND_MS)
            if (pendingConnect != pending) return@LaunchedEffect
            // Wall-clock budget, re-checked every round (#484 review):
            // round counting alone ignores IO time — a blackholed proxy
            // (30s read timeouts) would stretch 40 rounds past ~20 min,
            // past the 10-min attempt TTL, polling a dead single-use
            // attempt toward a "Still waiting" that can never resolve.
            // pendingSince == 0 only for waits predating it: those keep
            // the round count.
            if (pendingSince != 0L &&
                SystemClock.uptimeMillis() - pendingSince >= POLL_BUDGET_MS
            ) {
                break
            }
            // Freshness anchor, like refresh: overlapping writers resolve
            // by fetch order (#484 review).
            val roundStart = stampFetch()
            ConnectorsApi(context).list(path()).fold(
                onSuccess = { list ->
                    // Wait key re-checked BEFORE applying (#484 review): a
                    // disconnect landing mid-IO must not have its mapping
                    // overwritten by this stale round — and with the wait
                    // cancelled there is no later round to heal it. Refresh
                    // resolutions and newer waits own their own applies.
                    if (pendingConnect != pending) return@LaunchedEffect
                    // Data apply is freshness-guarded AND resolution rides
                    // with it: resolving off a vetoed payload would toast
                    // over contradictory rows with no later round to heal
                    // (#484 review). A vetoed flip heals via the still-open
                    // wait's next round instead.
                    if (claimFresh(roundStart)) {
                        connectors = list
                        // Same stale-banner rule as refresh() above: a good
                        // poll round heals a failed manual refresh (#484
                        // review).
                        error = ""
                        // Connected-only: a pre-existing expired row must
                        // not end the wait on round 1 (#484 review).
                        if (list.any { it.id == pending && it.isConnected() }) {
                            pendingConnect = null
                            toasts.toast("Connected.", kind = ToastKind.Success)
                        }
                    }
                    // Verdict accumulates into the saveable record, not
                    // locals: the budget survives rotation, so the verdict
                    // must too — otherwise healthy pre-rotate rounds are
                    // forgotten and the epilogue misclassifies (#484
                    // review). Error only when NO round succeeded.
                    pollReached = true
                    pollLastError = ""
                },
                onFailure = { e ->
                    pollLastError = e.message ?: e.javaClass.simpleName
                },
            )
        }
        // Wait ran out with the row still unchanged: stop polling, keep
        // the manual check. A wait that never reached the server is a
        // dead server, not a slow consent; anything else is still just
        // waiting on the browser.
        if (pendingConnect == pending) {
            pendingConnect = null
            if (pollReached) {
                toasts.toast("Still waiting — tap Check again after finishing in the browser.")
            } else {
                toasts.toast(
                    pollLastError.ifEmpty { "Couldn't reach the server." },
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
                                        pendingSince = SystemClock.uptimeMillis()
                                        // Fresh wait, fresh verdict: stale
                                        // verdicts from a previous wait must
                                        // not leak into this one's epilogue.
                                        pollReached = false
                                        pollLastError = ""
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
                                        // Optimistic row map: the POST confirmed
                                        // this row's state, so it never reads
                                        // stale whatever the re-list below does
                                        // (#484 review). Mirrors the server's
                                        // disconnected shape (tokens gone,
                                        // lastError cleared, no reconnect
                                        // hint — that rides expired rows
                                        // only). Stamped fresh so every
                                        // started fetch reads strictly older:
                                        // advancing merely to the newest
                                        // stamp would tie it, and ties pass
                                        // (#484 review).
                                        connectors = connectors.map {
                                            if (it.id == c.id) {
                                                it.copy(
                                                    status = "disconnected",
                                                    lastError = "",
                                                    reconnect = "",
                                                )
                                            } else {
                                                it
                                            }
                                        }
                                        // Stamp outside the update lambda: it may
                                        // retry on contention, and every
                                        // evaluation would mint (and skip) a
                                        // sequence.
                                        val fresh = stampFetch()
                                        appliedSeq.getAndUpdate { cur ->
                                            maxOf(cur, fresh)
                                        }
                                        // Full re-list inline for the REST, not
                                        // a tick bump (#484 review): a tick
                                        // could CAS-fail on our own held guard
                                        // and heal nothing. Rows and banner
                                        // follow the same freshness gate as
                                        // every other apply; the success toast
                                        // below names the confirmed POST, not
                                        // the re-list, so a vetoed re-list
                                        // stays silent while fresher data
                                        // already shows the row.
                                        val regen = stampFetch()
                                        ConnectorsApi(context).list(path()).fold(
                                            onSuccess = { list ->
                                                if (claimFresh(regen)) {
                                                    connectors = list
                                                    error = ""
                                                }
                                            },
                                            onFailure = { e ->
                                                if (claimFresh(regen)) {
                                                    error = e.message ?: e.javaClass.simpleName
                                                }
                                            },
                                        )
                                        toasts.toast(
                                            "Disconnected — future access stopped.",
                                            kind = ToastKind.Success,
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
