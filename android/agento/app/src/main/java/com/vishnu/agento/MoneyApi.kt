package com.vishnu.agento

import android.content.Context
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import java.util.concurrent.TimeUnit
import androidx.compose.runtime.Immutable
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** One money account with its derived balance in paise (int64 — never a
 * float on the wire; formatting to rupees happens only for display). */
@Immutable
data class MoneyAccount(
    val id: String,
    val name: String,
    val type: String = "",
    val archived: Boolean = false,
    val balancePaise: Long = 0L,
)

/** One ledger row. `amountPaise` is signed by display convention only at
 * render time — storage carries the raw amount plus `type`. */
@Immutable
data class MoneyTransaction(
    val id: String,
    val date: String = "",
    val type: String = "",
    val category: String = "",
    val account: String = "",
    val sendingToAccount: String = "",
    val amountPaise: Long = 0L,
    val note: String = "",
)

internal fun parseMoneyAccount(o: JSONObject): MoneyAccount? {
    val id = optJsonStr(o, "id")
    if (id.isEmpty()) return null
    return MoneyAccount(
        id = id,
        name = optJsonStr(o, "name"),
        type = optJsonStr(o, "type"),
        archived = o.optBoolean("archived", false),
        balancePaise = o.optLong("balance_paise", 0L),
    )
}

internal fun parseMoneyTransaction(o: JSONObject): MoneyTransaction? {
    val id = optJsonStr(o, "id")
    if (id.isEmpty()) return null
    return MoneyTransaction(
        id = id,
        date = optJsonStr(o, "date"),
        type = optJsonStr(o, "type"),
        category = optJsonStr(o, "category"),
        account = optJsonStr(o, "account"),
        sendingToAccount = optJsonStr(o, "sending_to_account"),
        amountPaise = o.optLong("amount_paise", 0L),
        note = optJsonStr(o, "note"),
    )
}

/** Parses the balances envelope: the canonical `accounts` list plus
 * `total_paise`. A missing envelope is a shape error, never a clean zero. */
internal fun parseMoneyBalances(body: String): Pair<List<MoneyAccount>, Long> {
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(MONEY_SHAPE_ERROR)
    val arr = root.optJSONArray("accounts")
        ?: throw RuntimeException(MONEY_SHAPE_ERROR)
    val out = mutableListOf<MoneyAccount>()
    for (i in 0 until arr.length()) {
        val o = arr.optJSONObject(i) ?: continue
        parseMoneyAccount(o)?.let { out.add(it) }
    }
    return out to root.optLong("total_paise", 0L)
}

/** One page of ledger rows plus the window totals, so the UI can say
 * showing-N-of-M instead of reading as complete. */
data class MoneyPage(
    val rows: List<MoneyTransaction>,
    val total: Long = 0L,
    val truncated: Boolean = false,
)

/** Parses the transactions envelope: the canonical `transactions` list plus
 * `total`/`truncated` for the showing-N-of-M indicator. A missing envelope
 * is a shape error, never a clean empty that reads as no rows. */
internal fun parseMoneyTransactions(body: String): MoneyPage {
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(MONEY_SHAPE_ERROR)
    val arr = root.optJSONArray("transactions")
        ?: throw RuntimeException(MONEY_SHAPE_ERROR)
    val out = mutableListOf<MoneyTransaction>()
    for (i in 0 until arr.length()) {
        val o = arr.optJSONObject(i) ?: continue
        parseMoneyTransaction(o)?.let { out.add(it) }
    }
    return MoneyPage(out, root.optLong("total", out.size.toLong()), root.optBoolean("truncated", false))
}

/** One message for a server that does not speak the money shape: names the
 * missing envelope and the fix, never a clean empty that reads as broke. */
internal const val MONEY_SHAPE_ERROR =
    "Unexpected response shape — the server sent no money data. Update the server."

/** Formats int64 paise as Indian rupees for display only (₹1,234.56).
 * Ledger math stays in paise everywhere else. */
internal fun formatPaise(paise: Long): String =
    java.text.NumberFormat.getCurrencyInstance(java.util.Locale("en", "IN"))
        .format(paise / 100.0)

/**
 * Money client over health-api (`/api/money`, same bearer auth as the other
 * health-api clients). Read-only in v1: balances plus a paged transaction
 * list. No log/edit/delete paths exist — those ride the agent's MCP tools.
 */
class MoneyApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        /** Path encoding (`+` is a space in form encoding, not in a path). */
        internal fun encode(v: String): String =
            java.net.URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")
    }

    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

    private fun call(url: String): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
        }
        return runCatching {
            val request = Request.Builder()
                .url(base + url)
                .header("Authorization", "Bearer ${password()}")
                .get()
                .build()
            http.newCall(request).execute().use { response ->
                val text = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }

    /** Current per-account balances plus the active total, both in paise. */
    suspend fun balances(): Result<Pair<List<MoneyAccount>, Long>> =
        withContext(Dispatchers.IO) {
            call("/api/money/balances").mapCatching { parseMoneyBalances(it) }
        }

    /** One page of the ledger, newest first. The server pages from the
     * newest end on `order=desc`, so the first page is the recent rows —
     * with `total`/`truncated` saying whether older rows remain. */
    suspend fun transactions(
        start: String,
        end: String,
        account: String? = null,
        limit: Int = 100,
        offset: Int = 0,
    ): Result<MoneyPage> =
        withContext(Dispatchers.IO) {
            val url = buildString {
                append("/api/money/transactions?start=${encode(start)}&end=${encode(end)}")
                append("&limit=$limit&offset=$offset&order=desc")
                if (!account.isNullOrBlank()) append("&account=${encode(account)}")
            }
            call(url).mapCatching { parseMoneyTransactions(it) }
        }
}
