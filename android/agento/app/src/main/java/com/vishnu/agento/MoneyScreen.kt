@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Money section (#571): read-only balances plus recent transactions.
 * Balances are derived server-side (`GET /api/money/balances`); tapping an
 * account lists its recent rows (`GET /api/money/transactions`). No
 * log/edit/delete paths exist here — those ride the agent's MCP tools.
 * Amounts render from int64 paise via [formatPaise]; no float math feeds
 * the ledger (display only).
 */

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBalanceWallet
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch

@Composable
internal fun MoneyScreen(
    onMenu: () -> Unit = {},
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var accounts by remember { mutableStateOf<List<MoneyAccount>>(emptyList()) }
    var totalPaise by remember { mutableStateOf(0L) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var openAccount by remember { mutableStateOf<MoneyAccount?>(null) }

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            MoneyApi(context).balances().fold(
                onSuccess = {
                    accounts = it.first
                    totalPaise = it.second
                    loaded = true
                },
                onFailure = { e ->
                    error = e.message ?: e.javaClass.simpleName
                    loaded = true
                },
            )
            busy = false
        }
    }

    LaunchedEffect(Unit) { load() }

    // Account drill-in is in-screen state (not a destination): back returns
    // to balances without touching navigation.
    val open = openAccount
    if (open != null) {
        MoneyTransactionsView(
            account = open,
            onBack = { openAccount = null },
        )
        return
    }

    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Money") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { load() }, enabled = !busy) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            // A failed refresh keeps old balances but says so — stale data
            // must not read as fresh.
            if (error.isNotEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            if (loaded && accounts.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.AccountBalanceWallet,
                    title = "No accounts yet",
                    subtitle = "Accounts are created by the agent. Balances appear here once they exist.",
                )
            }
            if (accounts.isNotEmpty()) {
                Card(modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
                    Row(
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Text(
                            "Total (active)",
                            style = MaterialTheme.typography.bodyLarge,
                            modifier = Modifier.weight(1f),
                        )
                        Text(
                            formatPaise(totalPaise),
                            style = MaterialTheme.typography.bodyLarge.copy(fontWeight = FontWeight.Bold),
                        )
                    }
                }
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(accounts, key = { it.id }) { account ->
                    Card(
                        onClick = { openAccount = account },
                        modifier = Modifier.fillMaxWidth(),
                    ) {
                        Row(
                            modifier = Modifier.fillMaxWidth().padding(12.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Column(modifier = Modifier.weight(1f)) {
                                Text(
                                    account.name.ifEmpty { "(unnamed account)" },
                                    style = MaterialTheme.typography.bodyLarge,
                                    maxLines = 1,
                                    overflow = TextOverflow.Ellipsis,
                                )
                                val sub = buildString {
                                    if (account.type.isNotEmpty()) append(account.type)
                                    if (account.archived) {
                                        if (isNotEmpty()) append(" · ")
                                        append("archived")
                                    }
                                }
                                if (sub.isNotEmpty()) {
                                    Text(
                                        sub,
                                        style = MaterialTheme.typography.labelSmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                            }
                            Text(
                                formatPaise(account.balancePaise),
                                style = MaterialTheme.typography.bodyLarge.copy(fontWeight = FontWeight.Bold),
                            )
                        }
                    }
                }
            }
        }
    }
    }
}

@Composable
private fun MoneyTransactionsView(
    account: MoneyAccount,
    onBack: () -> Unit,
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var rows by remember { mutableStateOf<List<MoneyTransaction>>(emptyList()) }
    var total by remember { mutableStateOf(0L) }
    var truncated by remember { mutableStateOf(false) }
    var nextOffset by remember { mutableStateOf<Int?>(null) }
    var error by remember { mutableStateOf("") }
    var appendError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var appending by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }

    fun load(silent: Boolean = false, offset: Int = 0, append: Boolean = false) {
        // Appends track their own in-flight flag: the button stays enabled
        // while busy is untouched on the silent path, so a second tap would
        // fire a duplicate page (same offset appended twice, ids doubled in
        // a keyed list). The shared busy flag is only for full loads, so a
        // finished append never hides an in-flight refresh either.
        if (append) {
            if (appending) return
            appending = true
        } else if (!silent) {
            busy = true
            error = ""
            appendError = ""
        }
        scope.launch {
            // Rows never expire: a wide window with paging. Newest first
            // (server order=desc), 100 per page — the subtitle counts
            // whether older rows remain, and an explicit control fetches
            // them via offset=next_offset (no silent dead end at 100).
            // Filtered by stable account id, so a rename never breaks the
            // drill-in the way a cached name would. The window is
            // 1970-01-01..2100-12-31: balances derive from all live rows
            // with no date cap, so a bounded window would hide
            // out-of-window rows the total includes.
            MoneyApi(context).transactions(
                start = "1970-01-01",
                end = "2100-12-31",
                account = account.id,
                limit = 100,
                offset = offset,
            ).fold(
                onSuccess = { page ->
                    rows = if (append) rows + page.rows else page.rows
                    total = page.total
                    truncated = page.truncated
                    nextOffset = if (page.truncated) offset + page.rows.size else null
                    if (append) appendError = "" else error = ""
                    loaded = true
                },
                onFailure = { e ->
                    val msg = e.message ?: e.javaClass.simpleName
                    if (append) appendError = msg else error = msg
                    loaded = true
                },
            )
            if (append) appending = false else busy = false
        }
    }

    LaunchedEffect(account.id) { load() }

    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(account.name, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
                actions = {
                    IconButton(onClick = { load() }, enabled = !busy) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            // A failed refresh keeps old rows but says so — stale data must
            // not read as fresh.
            if (error.isNotEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            if (loaded && rows.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.AccountBalanceWallet,
                    title = "No transactions",
                    subtitle = "Rows logged on ${account.name} appear here, newest first.",
                )
            }
            if (loaded && rows.isNotEmpty() && error.isEmpty()) {
                val showing = if (truncated) {
                    "Showing newest ${rows.size} of $total"
                } else {
                    val n = rows.size
                    "$n transaction" + if (n == 1) "" else "s"
                }
                Text(
                    showing,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp),
                )
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(rows, key = { it.id }) { row ->
                    Card(modifier = Modifier.fillMaxWidth()) {
                        Row(
                            modifier = Modifier.fillMaxWidth().padding(12.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Column(modifier = Modifier.weight(1f)) {
                                val title = when (row.type) {
                                    "transfer" -> {
                                        val dst = row.sendingToAccount.ifEmpty { "?" }
                                        "${row.account} → $dst"
                                    }
                                    else -> row.category.ifEmpty { row.type }
                                }
                                Text(
                                    title.ifEmpty { "(untitled)" },
                                    style = MaterialTheme.typography.bodyLarge,
                                    maxLines = 1,
                                    overflow = TextOverflow.Ellipsis,
                                )
                                val meta = buildString {
                                    if (row.date.isNotEmpty()) append(row.date)
                                    if (row.note.isNotEmpty()) {
                                        if (isNotEmpty()) append(" · ")
                                        append(row.note)
                                    }
                                }
                                if (meta.isNotEmpty()) {
                                    Text(
                                        meta,
                                        style = MaterialTheme.typography.labelSmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        maxLines = 2,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                }
                            }
                            val signed = when (row.type) {
                                "income" -> "+${formatPaise(row.amountPaise)}"
                                "expense" -> "-${formatPaise(row.amountPaise)}"
                                else -> formatPaise(row.amountPaise)
                            }
                            Text(
                                signed,
                                style = MaterialTheme.typography.bodyLarge.copy(fontWeight = FontWeight.Bold),
                                color = when (row.type) {
                                    "income" -> MaterialTheme.colorScheme.primary
                                    "expense" -> MaterialTheme.colorScheme.error
                                    else -> MaterialTheme.colorScheme.onSurface
                                },
                            )
                        }
                    }
                }
                // Older rows are one tap away, not a dead end: the server
                // pages newest-first, so offset=next_offset continues where
                // this page stopped and appends below. A failed append keeps
                // its own retry inline instead of hiding the button; an
                // in-flight append disables it (plus inline progress) so a
                // double-tap cannot append the same page twice.
                val more = nextOffset
                if (loaded && truncated && error.isEmpty() && more != null) {
                    item {
                        if (appendError.isEmpty()) {
                            if (appending) {
                                LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                            } else {
                                OutlinedButton(
                                    onClick = { load(silent = true, offset = more, append = true) },
                                    enabled = !busy,
                                    modifier = Modifier.fillMaxWidth(),
                                ) {
                                    Text("Show older transactions")
                                }
                            }
                        } else {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Text(
                                    "Couldn't load older transactions.",
                                    style = MaterialTheme.typography.labelMedium,
                                    color = MaterialTheme.colorScheme.error,
                                    modifier = Modifier.weight(1f),
                                )
                                TextButton(
                                    onClick = { load(silent = true, offset = more, append = true) },
                                    enabled = !busy,
                                ) {
                                    Text("Retry")
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    }
}
