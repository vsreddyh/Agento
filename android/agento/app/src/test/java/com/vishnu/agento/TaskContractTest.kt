package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * #185: the app trusted every response, so a contract mismatch looked like a bug in
 * the app rather than a disagreement between versions.
 *
 * The historical case: `POST /api/tasks/{id}/complete` changed shape during the 4.7.0
 * review. The shipped fix kept the task at the top level and added `next`, so old
 * clients kept working — which is exactly why nothing caught the break. The old parser
 * read the old shape, set `nextDueDate = ""`, concluded the task had not rolled over,
 * and opened a recreate draft asking the user for a date that already existed. Silent,
 * and plausible enough to look like a rollover bug.
 *
 * These tests pin the comparison itself, on the layer that decides — not on a helper
 * beside the code that calls it, which is the failure mode this suite has caught before.
 */
class TaskContractTest {

    /**
     * What the app asked the server to store.
     *
     * This is the PRODUCTION type, not a local copy. A test that declared its own
     * struct with the same field names would compile and pass while proving nothing
     * about the type the app actually sends — and a rename on either side would leave
     * the test green.
     */
    /** The canonical "daily chore" every fixture below starts from. */
    private fun daily() = TaskContractSent(
        name = "Take meds",
        description = "with water",
        dueDate = "2026-10-06",
        dueTime = "09:00",
        estimatedMinutes = 5,
        repeatEvery = 1,
        repeatUnit = "days",
        repeatCustom = false,
        repeatRule = "",
        parallelable = false,
    )

    /**
     * A server that agreed with everything.
     *
     * `TaskContractSent` fields are nullable (null = "not asserted"), `ServerTask`'s are
     * not, so each is unwrapped here. `?: ""` and `?: 0` are FINE in this direction —
     * the server always stores a value — and they are not the falsy-default trap the
     * production code avoids, because a null reaching this function means the test asked
     * for a comparison of a field it never set.
     */
    private fun stored(s: TaskContractSent) = ServerTask(
        id = "6abf0000000000000000abcd",
        name = s.name ?: "",
        description = s.description ?: "",
        dueDate = s.dueDate ?: "",
        dueTime = s.dueTime ?: "",
        estimatedMinutes = s.estimatedMinutes ?: 0,
        repeatEvery = s.repeatEvery ?: 0,
        repeatUnit = s.repeatUnit ?: "",
        repeatCustom = s.repeatCustom ?: false,
        repeatRule = s.repeatRule ?: "",
        parallelable = s.parallelable ?: false,
    )

    @Test
    fun `a response that matches the request reports nothing`() {
        val s = daily()
        assertEquals(emptyList<String>(), contractMismatches(s, stored(s)))
    }

    @Test
    fun `a cadence silently dropped by the server is reported`() {
        // The 4.6.0 shape: the app asked for a daily repeat and got a one-shot task
        // back. Every field defaulted, so nothing looked wrong.
        val got = stored(daily()).copy(repeatEvery = 0, repeatUnit = "")
        val m = contractMismatches(daily(), got)
        assertTrue("expected the cadence to be reported, got $m", m.any { it.contains("repeat") })
    }

    @Test
    fun `a shifted due date is reported even though the task exists`() {
        // The recreate-draft case: the record is there, and the date is wrong.
        val got = stored(daily()).copy(dueDate = "2026-10-07")
        val m = contractMismatches(daily(), got)
        assertEquals(listOf("due_date"), m)
    }

    @Test
    fun `every disagreeing field is named, not just the first`() {
        // A single "something differed" would send the user hunting; the field names
        // are the whole value of this check.
        val got = stored(daily()).copy(
            dueTime = "10:00",
            repeatEvery = 2,
            parallelable = true,
        )
        val m = contractMismatches(daily(), got)
        assertEquals(
            listOf("due_time", "repeat_every", "parallelable"),
            m,
        )
    }

    @Test
    fun `whitespace differences are not a contract break`() {
        // The app trims before sending and the server trims before storing, so a
        // padded value agreeing after trim is agreement. Flagging it would train
        // people to ignore the warning.
        val got = stored(daily()).copy(name = "  Take meds  ", repeatUnit = " days ")
        assertEquals(emptyList<String>(), contractMismatches(daily(), got))
    }

    @Test
    fun `a custom rule is compared as text`() {
        val s = daily().copy(
            repeatCustom = true, repeatEvery = 0, repeatUnit = "",
            repeatRule = "every sunday",
        )
        assertEquals(emptyList<String>(), contractMismatches(s, stored(s)))
        val got = stored(s).copy(repeatRule = "every other sunday")
        assertEquals(listOf("repeat_rule"), contractMismatches(s, got))
    }

    @Test
    fun `an empty id is a failure the caller must not treat as success`() {
        // parseOne already throws on an unreadable shape, but a body that parses to a
        // task with no id is a different failure and must not pass as a created task.
        assertTrue(
            "a response with no id cannot be reported as a successful write",
            contractMismatches(daily(), stored(daily()).copy(id = "")).contains("id"),
        )
    }

    @Test
    fun `a null field is not asserted at all`() {
        // A partial update knows nothing about the fields it did not send. Reporting
        // them would fail every single edit, so null must mean "skip", not "empty".
        val got = stored(daily()).copy(dueTime = "23:59", repeatUnit = "fortnights")
        // Assert only the name; everything else is unknown and must be skipped.
        assertEquals(emptyList<String>(), contractMismatches(TaskContractSent(name = "Take meds"), got))
    }

    @Test
    fun `a dropped description is reported`() {
        // create() sends description, so a server that dropped it is a real break. It
        // was silently unasserted in the first cut of this PR.
        val got = daily().let { stored(it) }.copy(description = "")
        assertTrue(
            contractMismatches(daily(), got).contains("description"),
        )
    }

    @Test
    fun `zero is a real value and must not be reported as absent`() {
        // estimated_minutes 0 is legal and means "no estimate". A check written with
        // falsy defaults would call it missing on every one-shot task.
        val s = daily().copy(estimatedMinutes = 0, repeatEvery = 0, repeatUnit = "")
        assertEquals(emptyList<String>(), contractMismatches(s, stored(s)))
    }
}

/**
 * The warning sink, and the guarantee that a report actually becomes visible.
 *
 * A mismatch raised while no screen is collecting it used to be the shape of the bug:
 * the app had the information and there was nowhere for it to go, so the user saw a
 * task that looked right and was not.
 */
class ContractWarningsTest {

    private fun reset() = ContractWarnings.consume(ContractWarnings.mismatched.value)

    @Test
    fun `an empty mismatch list is never reported`() {
        reset()
        ContractWarnings.report(emptyList())
        assertEquals(emptyList<String>(), ContractWarnings.mismatched.value)
    }

    @Test
    fun `the newest mismatch replaces the previous one`() {
        // A burst of editor autosaves would otherwise queue a stack of stale
        // warnings the user never dismisses; only the newest describes reality.
        reset()
        ContractWarnings.report(listOf("due_time"))
        ContractWarnings.report(listOf("repeat_every", "repeat_unit"))
        assertEquals(listOf("repeat_every", "repeat_unit"), ContractWarnings.mismatched.value)
    }

    @Test
    fun `generation advances even for an identical repeat`() {
        // Otherwise "the same warning twice in a row" is indistinguishable from the
        // app being stuck, and a genuine second failure looks like a duplicate.
        reset()
        val before = ContractWarnings.generation
        ContractWarnings.report(listOf("due_date"))
        ContractWarnings.report(listOf("due_date"))
        assertEquals(before + 2, ContractWarnings.generation)
    }

    @Test
    fun `the message names every field`() {
        // "Something differs" tells the user nothing they can act on.
        val msg = ContractWarnings.message(listOf("due_date", "repeat_every"))
        assertTrue(msg, msg.contains("due_date"))
        assertTrue(msg, msg.contains("repeat_every"))
        assertTrue(msg, msg.contains("different versions"))
    }

    @Test
    fun `the message says the write was saved`() {
        // The write DID succeed. Wording that implies otherwise would make the user
        // re-enter a task that is already stored — the same duplicate-work problem
        // #214 is about.
        val msg = ContractWarnings.message(listOf("due_date"))
        assertTrue(msg, msg.startsWith("Saved, but"))
    }

    @Test
    fun `consume clears the warning without touching the generation`() {
        reset()
        ContractWarnings.report(listOf("due_date"))
        val gen = ContractWarnings.generation
        ContractWarnings.consume(listOf("due_date"))
        assertEquals(emptyList<String>(), ContractWarnings.mismatched.value)
        assertEquals(gen, ContractWarnings.generation)
    }

    @Test
    fun `consume does not wipe a report that arrived while the snackbar was up`() {
        // The lost-warning bug: showSnackbar SUSPENDS, so a second report can land while
        // the first is still on screen, and an unconditional clear wipes the second one
        // before it is ever shown. Two saves in quick succession is exactly when a
        // contract mismatch is most likely — both are hitting the same broken server.
        reset()
        ContractWarnings.report(listOf("due_time"))
        val shown = ContractWarnings.mismatched.value
        // ... snackbar is up, and a second write disagrees about something else ...
        ContractWarnings.report(listOf("repeat_every"))
        assertEquals(listOf("repeat_every"), ContractWarnings.mismatched.value)
        // ... the first snackbar finishes and the collector clears what it showed.
        ContractWarnings.consume(shown)
        assertEquals(
            "the second warning was wiped by the first one's cleanup",
            listOf("repeat_every"),
            ContractWarnings.mismatched.value,
        )
    }

    @Test
    fun `consume of the current value does clear it`() {
        // The fix must not turn into a warning that never goes away.
        reset()
        ContractWarnings.report(listOf("due_time"))
        ContractWarnings.consume(ContractWarnings.mismatched.value)
        assertEquals(emptyList<String>(), ContractWarnings.mismatched.value)
    }

    @Test
    fun `the warning survives being read from another thread`() {
        // report() runs on Dispatchers.IO and the screen collects on the main thread.
        // With plain `var`s there is no happens-before edge between them, so the UI
        // could miss the write entirely — which is the failure this sink had.
        reset()
        val fields = listOf("repeat_every", "repeat_unit")
        val writer = Thread { ContractWarnings.report(fields) }
        writer.start()
        writer.join()
        assertEquals(fields, ContractWarnings.mismatched.value)
    }
}
