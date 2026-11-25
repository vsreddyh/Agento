package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Money read surface (#571).
 *
 * Row fields are lenient by contract — missing keys read blank/zero, corrupt
 * id-less rows are skipped. The envelopes are strict: `accounts` and
 * `transactions` are the only shapes that parse, and amounts stay int64
 * paise (no float crosses the wire).
 */
class MoneyApiTest {

    @Test
    fun `parseMoneyAccount reads full record`() {
        val a = parseMoneyAccount(
            JSONObject(
                """{"id":"abc","name":"Cash","type":"cash",""" +
                    """"archived":false,"balance_paise":12345}""",
            ),
        )!!
        assertEquals("abc", a.id)
        assertEquals("Cash", a.name)
        assertEquals("cash", a.type)
        assertEquals(false, a.archived)
        assertEquals(12345L, a.balancePaise)
    }

    @Test
    fun `parseMoneyAccount drops id-less rows and blanks`() {
        assertNull(parseMoneyAccount(JSONObject("""{"name":"Cash"}""")))
        val a = parseMoneyAccount(JSONObject("""{"id":"abc"}"""))!!
        assertEquals("", a.name)
        assertEquals(0L, a.balancePaise)
    }

    @Test
    fun `parseMoneyTransaction reads full record`() {
        val t = parseMoneyTransaction(
            JSONObject(
                """{"id":"r1","date":"2026-09-01","type":"expense",""" +
                    """"category":"groceries","account":"Cash",""" +
                    """"amount_paise":250050,"note":"lunch"}""",
            ),
        )!!
        assertEquals("r1", t.id)
        assertEquals("2026-09-01", t.date)
        assertEquals("groceries", t.category)
        assertEquals(250050L, t.amountPaise)
        assertEquals("lunch", t.note)
    }

    @Test
    fun `parseMoneyBalances reads envelope and total`() {
        val (accounts, total) = parseMoneyBalances(
            """{"ok":true,"total_paise":50000,"accounts":[""" +
                """{"id":"a","name":"Cash","balance_paise":20000},""" +
                """{"id":"b","name":"Bank","balance_paise":30000}]}""",
        )
        assertEquals(2, accounts.size)
        assertEquals(50000L, total)
        assertEquals(20000L, accounts[0].balancePaise)
    }

    @Test
    fun `parseMoneyBalances rejects missing envelope`() {
        try {
            parseMoneyBalances("""{"ok":true,"total_paise":0}""")
            assertTrue("missing accounts envelope was accepted", false)
        } catch (e: RuntimeException) {
            assertTrue(e.message!!.contains("money data"))
        }
    }

    @Test
    fun `formatPaise renders rupees with paise precision`() {
        // Display only — the ledger never sees this float.
        val s = formatPaise(250050L)
        assertTrue("got $s", s.contains("2,500.50"))
    }

    @Test
    fun `parseMoneyTransactions reads envelope with totals`() {
        val page = parseMoneyTransactions(
            """{"ok":true,"count":1,"total":250,"truncated":true,""" +
                """"transactions":[{"id":"r1","date":"2026-09-01","type":"expense",""" +
                """"category":"groceries","amount_paise":250050}]}""",
        )
        assertEquals(1, page.rows.size)
        assertEquals(250050L, page.rows[0].amountPaise)
        assertEquals(250L, page.total)
        assertEquals(true, page.truncated)
    }

    @Test
    fun `parseMoneyTransactions rejects missing footers`() {
        try {
            parseMoneyTransactions(
                """{"ok":true,"count":1,"transactions":[{"id":"r1"}]}""",
            )
            assertTrue("missing total/truncated was accepted", false)
        } catch (e: RuntimeException) {
            assertTrue(e.message!!.contains("money data"))
        }
    }

    @Test
    fun `parseMoneyBalances rejects missing total`() {
        try {
            parseMoneyBalances("""{"ok":true,"accounts":[{"id":"a"}]}""")
            assertTrue("missing total_paise was accepted", false)
        } catch (e: RuntimeException) {
            assertTrue(e.message!!.contains("money data"))
        }
    }

    @Test
    fun `parseMoneyBalances rejects wrong-typed total`() {
        // A numeric string coerces fine under optLong — strict means a real
        // JSON number, or the envelope reads as stale.
        try {
            parseMoneyBalances("""{"accounts":[],"total_paise":"50000"}""")
            assertTrue("string total_paise was accepted", false)
        } catch (e: RuntimeException) {
            assertTrue(e.message!!.contains("money data"))
        }
    }

    @Test
    fun `parseMoneyTransactions rejects wrong-typed footers`() {
        try {
            parseMoneyTransactions(
                """{"ok":true,"count":1,"transactions":[],"total":"5","truncated":"true"}""",
            )
            assertTrue("string total/truncated was accepted", false)
        } catch (e: RuntimeException) {
            assertTrue(e.message!!.contains("money data"))
        }
    }
}
