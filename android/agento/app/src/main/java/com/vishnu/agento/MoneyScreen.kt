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

private fun todayIst(): String =
    java.time.LocalDate.now(java.time.ZoneId.of("Asia/Kolkata")).toString()

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
            if (error.isNotEmpty() && accounts.isEmpty()) {
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
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            // Rows never expire: a wide window with paging is the honest
            // query. v1 shows the first page only (limit 100, date asc).
            MoneyApi(context).transactions(
                start = "2020-01-01",
                end = todayIst(),
                account = account.name,
            ).fold(
                onSuccess = {
                    rows = it
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
            if (error.isNotEmpty() && rows.isEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            if (loaded && rows.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.AccountBalanceWallet,
                    title = "No transactions",
                    subtitle = "Rows logged on ${account.name} appear here, oldest first.",
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
            }
        }
    }
    }
}
