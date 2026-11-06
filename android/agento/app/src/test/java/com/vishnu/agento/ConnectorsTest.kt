package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Connector list parsing (#440, #484 review): the server always returns
 * its rows, so a body without a `connectors` array is a loud shape
 * failure — never a clean empty that reads as "no connectors exist".
 * Id-less rows are still skipped individually.
 */
class ConnectorListParseTest {

    @Test
    fun `canonical envelope parses rows`() {
        val rows = parseConnectorList(
            """{"connectors":[
                {"id":"gmail","display_name":"Gmail","scopes":["gmail.read"],"status":"connected"},
                {"id":"calendar","status":"disconnected"}
            ]}""",
        )
        assertEquals(listOf("gmail", "calendar"), rows.map { it.id })
        assertEquals("Gmail", rows[0].displayName)
        assertEquals("calendar", rows[1].displayName)
        assertTrue(rows[0].isConnected())
    }

    @Test
    fun `id-less rows are skipped not fatal`() {
        val rows = parseConnectorList(
            """{"connectors":[{"name":"no id"},{"id":"gmail"}]}""",
        )
        assertEquals(listOf("gmail"), rows.map { it.id })
    }

    @Test
    fun `missing array fails loudly`() {
        try {
            parseConnectorList("""{"other":[]}""")
            throw AssertionError("must throw, not read as no connectors")
        } catch (e: RuntimeException) {
            assertEquals(CONNECTORS_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `garbage body fails loudly`() {
        try {
            parseConnectorList("not json at all")
            throw AssertionError("must throw, not read as no connectors")
        } catch (e: RuntimeException) {
            assertEquals(CONNECTORS_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `empty array fails loudly`() {
        // The server always returns its rows: [] is a wrong shape, not an
        // empty account (#484 review).
        try {
            parseConnectorList("""{"connectors":[]}""")
            throw AssertionError("must throw, not read as no connectors")
        } catch (e: RuntimeException) {
            assertEquals(CONNECTORS_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `all id-less rows fail loudly`() {
        try {
            parseConnectorList("""{"connectors":[{"name":"x"},{"id":""}]}""")
            throw AssertionError("must throw, not read as no connectors")
        } catch (e: RuntimeException) {
            assertEquals(CONNECTORS_SHAPE_ERROR, e.message)
        }
    }
}

/**
 * Consent-URL parsing (#484 review): the server fail-closes with 502,
 * so a 200 without a usable `auth_url` is a wrong shape, not an empty
 * grant — it must read as a shape error, never "empty auth_url".
 */
class ConnectAuthUrlTest {

    @Test
    fun `a consent url parses`() {
        assertEquals(
            "https://accounts.google.com/o/oauth2/auth?x=1",
            parseAuthUrl("""{"auth_url":"https://accounts.google.com/o/oauth2/auth?x=1"}"""),
        )
    }

    @Test
    fun `proxy html fails loudly`() {
        try {
            parseAuthUrl("<html>proxy</html>")
            throw AssertionError("must throw, not read as empty url")
        } catch (e: RuntimeException) {
            assertEquals(CONNECT_AUTH_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `missing or blank auth url fails loudly`() {
        for (body in listOf("""{"other":1}""", """{"auth_url":""}""", """{"auth_url":"   "}""")) {
            try {
                parseAuthUrl(body)
                throw AssertionError("must throw for $body")
            } catch (e: RuntimeException) {
                assertEquals(CONNECT_AUTH_SHAPE_ERROR, e.message)
            }
        }
    }
}

/**
 * Unconfigured-server detection (#484 review): matched on the status
 * code this client built, never on a "501" substring that could sit
 * anywhere in a truncated body.
 */
class ConnectNotConfiguredTest {

    @Test
    fun `a real 501 reads as not configured`() {
        assertTrue(isConnectNotConfigured("HTTP 501: this server has no OAuth config"))
    }

    @Test
    fun `a 501 inside another status body does not`() {
        assertFalse(isConnectNotConfigured("HTTP 500: scroll id 5017 failed"))
        assertFalse(isConnectNotConfigured("HTTP 502: {\"code\":501}"))
    }

    @Test
    fun `other failures read as themselves`() {
        assertFalse(isConnectNotConfigured("HTTP 404: unknown connector"))
        assertFalse(isConnectNotConfigured("empty auth_url"))
        assertFalse(isConnectNotConfigured(""))
    }
}
