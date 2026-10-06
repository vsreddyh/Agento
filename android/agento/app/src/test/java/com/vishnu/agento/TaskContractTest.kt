package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import java.util.concurrent.CopyOnWriteArrayList
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.yield
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Before
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
        // The fixture states the contract it speaks: a stored task with no
        // version is a pre-version server, which IS a mismatch (#185).
        contractVersion = TASK_CONTRACT_VERSION,
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

    @Before
    fun reset() {
        ContractWarnings.mismatched.value?.let { ContractWarnings.consume(it.generation) }
    }

    @Test
    fun `an empty mismatch list is never reported`() {
        ContractWarnings.report(emptyList())
        assertEquals(null, ContractWarnings.mismatched.value)
    }

    @Test
    fun `the newest mismatch replaces the previous one`() {
        // A burst of editor autosaves would otherwise queue a stack of stale
        // warnings the user never dismisses; only the newest describes reality.
        ContractWarnings.report(listOf("due_time"))
        ContractWarnings.report(listOf("repeat_every", "repeat_unit"))
        assertEquals(listOf("repeat_every", "repeat_unit"), ContractWarnings.mismatched.value?.fields)
    }

    @Test
    fun `generation advances even for an identical repeat`() {
        // Otherwise "the same warning twice in a row" is indistinguishable from the
        // app being stuck, and a genuine second failure looks like a duplicate.
        val before = ContractWarnings.generation
        ContractWarnings.report(listOf("due_date"))
        ContractWarnings.report(listOf("due_date"))
        assertEquals(before + 2, ContractWarnings.generation)
    }

    @Test
    fun `the message names the task`() {
        // Without it the warning is unactionable on a list screen: "Saved, but the server
        // stored different values for due_date" could belong to any row.
        val msg = ContractWarnings.message(listOf("due_date"), "Take meds")
        assertTrue(msg, msg.contains("Take meds"))
        assertTrue(msg, msg.contains("due_date"))
    }

    @Test
    fun `the message degrades when the task name is unknown`() {
        // checkContract always has a parsed ServerTask, so "" should not happen in
        // practice — but a name-less warning must still read as a sentence.
        val msg = ContractWarnings.message(listOf("due_date"))
        assertTrue(msg, msg.contains("due_date"))
        assertTrue(msg, !msg.contains("  "))
    }

    @Test
    fun `the task name rides along with the mismatch`() {
        reset()
        ContractWarnings.report(listOf("due_date"), "Take meds")
        assertEquals("Take meds", ContractWarnings.mismatched.value?.taskName)
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
        ContractWarnings.report(listOf("due_date"))
        val gen = ContractWarnings.generation
        ContractWarnings.consume(ContractWarnings.generation)
        assertEquals(null, ContractWarnings.mismatched.value)
        assertEquals(gen, ContractWarnings.generation)
    }

    @Test
    fun `consume does not wipe a report that arrived while the snackbar was up`() {
        // The lost-warning bug: showSnackbar SUSPENDS, so a second report can land while
        // the first is still on screen, and an unconditional clear wipes the second one
        // before it is ever shown. Two saves in quick succession is exactly when a
        // contract mismatch is most likely — both are hitting the same broken server.
        ContractWarnings.report(listOf("due_time"))
        val shownGen = ContractWarnings.mismatched.value!!.generation
        // ... snackbar is up, and a second write disagrees about something else ...
        ContractWarnings.report(listOf("repeat_every"))
        assertEquals(listOf("repeat_every"), ContractWarnings.mismatched.value?.fields)
        // ... the first snackbar finishes and the collector clears what it showed.
        ContractWarnings.consume(shownGen)
        assertEquals(
            "the second warning was wiped by the first one's cleanup",
            listOf("repeat_every"),
            ContractWarnings.mismatched.value?.fields,
        )
    }

    @Test
    fun `consume does not wipe an IDENTICAL report`() {
        // The fix for the above, and the case the list-comparison version got wrong:
        // `compareAndSet(shown, ...)` compares by EQUALITY, so two reports of the same
        // fields collapse to one value and clearing the first clears the second.
        //
        // This is the common case, not the exotic one — one broken server answers both
        // writes with the same disagreement, so same-fields is what actually happens.
        ContractWarnings.report(listOf("due_time"))
        val firstGen = ContractWarnings.mismatched.value!!.generation
        ContractWarnings.report(listOf("due_time"))
        val secondGen = ContractWarnings.mismatched.value!!.generation
        assertTrue(
            "two reports must be distinct even with identical fields, or clearing one " +
                "clears the other",
            secondGen != firstGen,
        )
        ContractWarnings.consume(firstGen)
        assertEquals(
            "the second identical warning was wiped by the first one's cleanup",
            listOf("due_time"),
            ContractWarnings.mismatched.value?.fields,
        )
    }

    /**
     * Two reports of the same fields must both reach a real collector.
     *
     * **No `consume` between them, and each emission rendezvoused** — report, wait for the
     * collector to have seen it, report again. Both halves are load-bearing:
     *
     * - Without the rendezvous, `StateFlow` holds only the NEWEST value, so a collector
     *   suspended across both reports wakes once and sees only the second. The test would
     *   fail on a CORRECT implementation.
     * - The rendezvous cannot return until a delivery has happened, which also covers a
     *   collector that subscribes LATE — `StateFlow` replays its current value to one.
     *
     * Earlier versions of this test failed both ways and passed while doing so; the
     * reasoning is in the PR thread rather than here, because the invariant above is what
     * the next reader needs.
     */
    @Test
    fun `two identical reports both reach a collector`() = runBlocking {
        // CopyOnWriteArrayList, not mutableListOf: the collector appends from its own
        // coroutine while the test thread reads `seen` in awaitEmissions. Safe today
        // under Dispatchers.Unconfined, which runs the collector on the caller's thread,
        // and silently unsafe the day the dispatcher changes — which is exactly the kind
        // of latent test failure that gets blamed on the code under test.
        val seen = CopyOnWriteArrayList<List<String>>()
        val started = CompletableDeferred<Unit>()
        val job = launch(Dispatchers.Unconfined) {
            ContractWarnings.mismatched.collect { m ->
                if (m != null) seen += m.fields
                // Signalled from INSIDE collect, not before it. Completing beforehand
                // only proves the coroutine started, not that collection is live — and
                // the awaitEmissions rendezvous below is what actually guarantees
                // delivery, since StateFlow replays its current value to a collector
                // that subscribes late.
                started.complete(Unit)
            }
        }
        try {
            started.await()
            ContractWarnings.report(listOf("due_time"))
            awaitEmissions(seen, 1)
            // Second report while the first is still the current value — the exact case
            // distinct-until-changed would swallow.
            ContractWarnings.report(listOf("due_time"))
            awaitEmissions(seen, 2)
            assertEquals(
                "the second identical report was conflated away — StateFlow suppresses an " +
                    "EQUAL value, so the payload must carry the generation to differ",
                listOf(listOf("due_time"), listOf("due_time")),
                seen,
            )
        } finally {
            job.cancel()
        }
    }

    /**
     * Suspends until the collector has seen [want] emissions, or fails the test.
     *
     * Bounded so a genuine regression cannot hang the suite: the wait gives up and the
     * assertion below reports the shortfall with the real count, which is a better
     * failure message than a timeout.
     */
    private suspend fun awaitEmissions(seen: List<*>, want: Int) {
        withTimeout(5_000) {
            while (seen.size < want) yield()
        }
    }

    @Test
    fun `consume between reports does not hide a second emission`() {
        // The previous shape, kept as its own assertion: the consume path is a separate
        // mechanism from the conflation path and both need to work.
        reset()
        ContractWarnings.report(listOf("due_time"))
        ContractWarnings.consume(ContractWarnings.generation)
        assertEquals(null, ContractWarnings.mismatched.value)
        ContractWarnings.report(listOf("due_time"))
        assertEquals(listOf("due_time"), ContractWarnings.mismatched.value?.fields)
    }

    @Test
    fun `consume of the current generation does clear it`() {
        // The fix must not turn into a warning that never goes away.
        ContractWarnings.report(listOf("due_time"))
        ContractWarnings.consume(ContractWarnings.generation)
        assertEquals(null, ContractWarnings.mismatched.value)
    }

    @Test
    fun `the warning survives being read from another thread`() {
        // report() runs on Dispatchers.IO and the screen collects on the main thread.
        // With plain `var`s there is no happens-before edge between them, so the UI
        // could miss the write entirely — which is the failure this sink had.
        val fields = listOf("repeat_every", "repeat_unit")
        val writer = Thread { ContractWarnings.report(fields) }
        writer.start()
        writer.join()
        assertEquals(fields, ContractWarnings.mismatched.value?.fields)
    }
}

/**
 * The `update()` call-site construction, not just the comparator.
 *
 * `TaskContractTest` above exercises `contractMismatches` directly, which leaves the
 * part most likely to rot untested: the `TaskContractSent` that `update` builds from its
 * nullable parameters. If that construction drifts — an untrimmed value, a field that
 * should be asserted left null, or a null that should be carried through — every
 * existing test still passes, because they all hand the comparator a hand-built value.
 *
 * That is the same shape as testing the parser while the producer drops the flag, which
 * is the bug #214 shipped with.
 */
class UpdateContractWiringTest {

    /**
     * The REAL construction, not a copy of it.
     *
     * This class originally hand-wrote its own version of `TasksApi.update`'s
     * `TaskContractSent(...)` call. That tested a COPY: if production drifted — an
     * untrimmed value, a field left null that should be asserted — every test here stayed
     * green while the wiring rotted. It is the fixture-duplication trap one level up from
     * the one this PR already fixed for `TaskContractSent` itself.
     *
     * Both `create` and `update` now build through `TasksApi.buildTaskContractSent`, so
     * the tests call the same function production does and a change to it fails here.
     */
    private fun updateSent(
        name: String? = null,
        description: String? = null,
        dueDate: String? = null,
        dueTime: String? = null,
        estimatedMinutes: Int? = null,
        repeatEvery: Int? = null,
        repeatUnit: String? = null,
        repeatCustom: Boolean? = null,
        repeatRule: String? = null,
        parallelable: Boolean? = null,
    ) = TasksApi.buildTaskContractSent(
        name = name,
        description = description,
        dueDate = dueDate,
        dueTime = dueTime,
        estimatedMinutes = estimatedMinutes,
        repeatEvery = repeatEvery,
        repeatUnit = repeatUnit,
        repeatCustom = repeatCustom,
        repeatRule = repeatRule,
        parallelable = parallelable,
    )

    private val agreed = ServerTask(
        id = "6abf0000000000000000abcd",
        name = "Take meds",
        dueDate = "2026-10-06",
        dueTime = "09:00",
        repeatEvery = 1,
        repeatUnit = "days",
        contractVersion = TASK_CONTRACT_VERSION,
    )

    @Test
    fun `an edit that sent nothing asserts nothing`() {
        // Every field null: nothing was sent, so nothing can be asserted. This is the
        // case that makes `update` usable at all — asserting the untouched fields would
        // report a mismatch on every single edit.
        assertEquals(emptyList<String>(), contractMismatches(updateSent(), agreed))
    }

    @Test
    fun `only the field that was sent is asserted`() {
        // The server changed due_time, which this edit did not touch. Not asserted, so
        // not reported — reporting it would fire on every concurrent write.
        val changedElsewhere = agreed.copy(dueTime = "23:59")
        assertEquals(
            emptyList<String>(),
            contractMismatches(updateSent(name = "Take meds"), changedElsewhere),
        )
    }

    @Test
    fun `the field that was sent IS asserted`() {
        val drifted = agreed.copy(name = "Something else")
        assertEquals(
            listOf("name"),
            contractMismatches(updateSent(name = "Take meds"), drifted),
        )
    }

    @Test
    fun `sent values are trimmed, so padding is not a false alarm`() {
        // The construction trims; if it stopped, the comparator's trim-insensitivity
        // would hide it and no other test would notice.
        assertEquals(
            emptyList<String>(),
            contractMismatches(updateSent(name = "  Take meds  "), agreed),
        )
    }

    @Test
    fun `a sent value the server changed is reported despite unsent neighbours`() {
        val drifted = agreed.copy(repeatUnit = "fortnights")
        assertEquals(
            listOf("repeat_unit"),
            contractMismatches(updateSent(repeatUnit = "days"), drifted),
        )
    }

    @Test
    fun `the create path asserts every field it sends`() {
        // create validates every field as non-blank, so nothing is null and everything
        // is asserted. If the builder ever stopped trimming, or create started passing a
        // null, this is the test that notices.
        val created = TasksApi.buildTaskContractSent(
            name = "  Take meds  ",
            description = "  with water  ",
            dueDate = " 2026-10-06 ",
            dueTime = " 09:00 ",
            estimatedMinutes = 5,
            repeatEvery = 1,
            repeatUnit = "  days ",
            repeatCustom = false,
            repeatRule = "",
            parallelable = false,
        )
        // `agreed` must carry the SAME values the create path sends, or this asserts
        // nothing useful: it first failed with `expected:<[]> but was:<[estimated_minutes]>`
        // because `agreed` left estimatedMinutes at its 0 default while the create sent 5.
        // A fixture that disagrees with the thing under test produces a test that either
        // fails for the wrong reason or passes for the wrong one.
        val storedByServer = agreed.copy(
            description = "with water",
            estimatedMinutes = 5,
        )
        assertEquals(emptyList<String>(), contractMismatches(created, storedByServer))
        // Nothing was left null by the create path.
        assertTrue(
            "create must assert every field it sends",
            listOf(
                created.name, created.description, created.dueDate, created.dueTime,
                created.repeatUnit, created.repeatRule,
            ).all { it != null },
        )
        assertEquals(5, created.estimatedMinutes)
        assertEquals(1, created.repeatEvery)
        assertEquals(false, created.repeatCustom)
        assertEquals(false, created.parallelable)
    }

    @Test
    fun `zero is asserted when sent`() {
        // `estimated_minutes = 0` is a real value meaning "no estimate", not an absence.
        val noEstimate = agreed.copy(estimatedMinutes = 0)
        assertEquals(
            emptyList<String>(),
            contractMismatches(updateSent(estimatedMinutes = 0), noEstimate),
        )
    }
}

/**
 * The record a write is ASSERTED from must be the record its body was WRITTEN from.
 *
 * `update` used to trim in two places — once for `body.put(...)` and once inside
 * `buildTaskContractSent` — which is the same value today and two rules tomorrow. Change
 * the builder's trimming and the body silently diverges, and the trim-insensitive compare
 * hides it, so the drift stays invisible until a server stops trimming on our behalf.
 *
 * Both write paths now build one `sent` and use it for both jobs. This asserts the shape
 * that makes that true: a partial edit leaves unsent fields null, which is what lets the
 * body omit them (a JSON null would read as an explicit clear) while the comparator skips
 * them.
 */
class SingleSourceOfTruthTest {

    private fun sent(
        name: String? = null,
        estimatedMinutes: Int? = null,
        repeatEvery: Int? = null,
        repeatCustom: Boolean? = null,
    ) = TasksApi.buildTaskContractSent(
        name = name, estimatedMinutes = estimatedMinutes,
        repeatEvery = repeatEvery, repeatCustom = repeatCustom,
    )

    @Test
    fun `an unsent field is null, so the body omits it and the compare skips it`() {
        val s = sent(name = "Take meds")
        assertEquals("Take meds", s.name)
        assertEquals(null, s.estimatedMinutes)
        assertEquals(null, s.repeatEvery)
        assertEquals(null, s.repeatCustom)
        // Null means "not asserted" — a JSON null would be an explicit clear.
        assertEquals(null, s.description)
    }

    @Test
    fun `a sent zero is preserved rather than collapsed to null`() {
        // The distinction that makes `?.let { body.put(...) }` safe: a null means NOT SENT,
        // and 0 means sent-and-zero. If the builder conflated them, an explicit "clear the
        // estimate" would silently stop being sent.
        val s = sent(estimatedMinutes = 0, repeatEvery = 0, repeatCustom = false)
        assertEquals(0, s.estimatedMinutes)
        assertEquals(0, s.repeatEvery)
        assertEquals(false, s.repeatCustom)
    }

    @Test
    fun `a sent blank string survives trimming as an empty string, not null`() {
        // Same reasoning as the zero case: "" was sent deliberately (clear the field) and
        // must not be mistaken for "not sent".
        assertEquals("", sent(name = "   ").name)
    }

    @Test
    fun `padding is trimmed once so body and assertion cannot diverge`() {
        assertEquals("Take meds", sent(name = "  Take meds  ").name)
    }
}

/**
 * Identity: a wrong-but-valid id is worse than a blank one.
 *
 * `parseTask` already rejects a blank id, so the blank arm is defence in depth. A server
 * answering with a DIFFERENT record is the case nothing else catches — every field can
 * match and the app stores a result for a task it never touched.
 */
class ContractIdentityTest {

    private val sent = TasksApi.buildTaskContractSent(name = "Take meds", dueDate = "2026-10-06")

    private fun stored(id: String) = ServerTask(
        id = id,
        name = "Take meds",
        dueDate = "2026-10-06",
        dueTime = "09:00",
        contractVersion = TASK_CONTRACT_VERSION,
    )

    @Test
    fun `a different id is reported`() {
        assertTrue(
            contractMismatches(sent, stored("6abf0000000000000000aaaa"), expectedId = "6abf0000000000000000bbbb")
                .contains("id"),
        )
    }

    @Test
    fun `the same id is not reported`() {
        val id = "6abf0000000000000000bbbb"
        assertEquals(
            emptyList<String>(),
            contractMismatches(sent, stored(id), expectedId = id),
        )
    }

    @Test
    fun `no expected id means identity is not asserted`() {
        // `create` has no id to expect — the server assigns it — so a mismatch must not
        // fire just because nothing was passed.
        assertEquals(emptyList<String>(), contractMismatches(sent, stored("6abf0000000000000000aaaa")))
    }

    @Test
    fun `a blank expected id is not asserted`() {
        // Defends against a caller passing "" and getting a permanent false mismatch.
        assertEquals(
            emptyList<String>(),
            contractMismatches(sent, stored("6abf0000000000000000aaaa"), expectedId = "  "),
        )
    }

    @Test
    fun `a blank returned id is still reported without an expected id`() {
        assertTrue(contractMismatches(sent, stored("")).contains("id"))
    }
}

/** A long task name must not push the field list off the snackbar. */
class TaskNameDisplayTest {

    @Test
    fun `a long name is elided for display`() {
        val name = "A".repeat(200)
        val msg = ContractWarnings.message(listOf("due_date"), name)
        // Assert the NAME is elided, not that the whole message fits some budget: the
        // sentence around it is legitimately long, and a total-length assertion fails for
        // reasons that have nothing to do with eliding — which is exactly what it did
        // before (expected < 200, got ~210, with the name correctly elided).
        assertTrue(
            "the 200-char name was shown verbatim",
            !msg.contains(name),
        )
        assertTrue("the name should be truncated to the display cap", msg.contains("\u2026"))
        assertTrue(
            "the elided name must still be roughly the cap, not the whole 200",
            msg.contains("A".repeat(ContractWarnings.NAME_DISPLAY_MAX - 1)),
        )
        // The whole point of the cap: the actionable half must survive.
        assertTrue("the field list must survive elision", msg.contains("due_date"))
    }

    @Test
    fun `a short name is shown in full`() {
        val msg = ContractWarnings.message(listOf("due_date"), "Take meds")
        assertTrue(msg.contains("Take meds"))
        assertTrue("nothing should be elided", !msg.contains("\u2026"))
    }

    @Test
    fun `elision keeps the name readable`() {
        val name = "B".repeat(ContractWarnings.NAME_DISPLAY_MAX + 50)
        val msg = ContractWarnings.message(listOf("due_date"), name)
        assertTrue(msg, msg.contains("\u2026"))
    }
}

/** A field must be reported once, however many of its checks failed. */
class ContractNoDuplicateFieldsTest {

    private val sent = TasksApi.buildTaskContractSent(name = "Take meds")

    @Test
    fun `a blank id with an expected id is reported once`() {
        // Both arms used to fire: blank `got.id` added "id", and a non-blank `expectedId`
        // that differs from a blank one added it again — so the message read "id, id".
        //
        // The fixture's name must MATCH `sent`, or `name` is a second genuine mismatch and
        // the assertion fails for a second reason. It did: `expected:<[id]> but
        // was:<[id, name]>` — the duplicate was fixed and my fixture was wrong.
        val got = contractMismatches(
            sent,
            ServerTask(id = "", name = "Take meds", contractVersion = TASK_CONTRACT_VERSION),
            expectedId = "6abfabc",
        )
        assertEquals(listOf("id"), got)
        assertEquals(1, got.count { it == "id" })
    }

    @Test
    fun `every reported field appears once`() {
        val got = contractMismatches(
            sent,
            ServerTask(
                id = "",
                name = "Something else",
                contractVersion = TASK_CONTRACT_VERSION,
            ),
            expectedId = "6abfabc",
        )
        assertEquals("a field must never be reported twice: $got", got.size, got.distinct().size)
    }
}

/**
 * The contract version itself is asserted (#185).
 *
 * The 4.6.0 structured-repeat change taught this: a response from an older
 * server defaulted every missing field to a genuine zero, so an un-updated
 * client rendered every structured cadence as a one-shot with nothing
 * warning. The version is what turns that silent default into a visible
 * disagreement — 0 means the server predates versions entirely, anything
 * above [TASK_CONTRACT_VERSION] means the server is newer than the app, and
 * either way the fields below may have been misread.
 */
class ContractVersionTest {

    private val sent = TasksApi.buildTaskContractSent(
        name = "Take meds",
        dueDate = "2026-10-06",
        dueTime = "09:00",
        estimatedMinutes = 5,
        repeatEvery = 1,
        repeatUnit = "days",
        repeatCustom = false,
        repeatRule = "",
        parallelable = false,
    )

    private fun stored(version: Int) = ServerTask(
        id = "6abf0000000000000000abcd",
        name = "Take meds",
        dueDate = "2026-10-06",
        dueTime = "09:00",
        estimatedMinutes = 5,
        repeatEvery = 1,
        repeatUnit = "days",
        repeatCustom = false,
        repeatRule = "",
        parallelable = false,
        contractVersion = version,
    )

    @Test
    fun `the version this app speaks reports nothing`() {
        assertEquals(
            emptyList<String>(),
            contractMismatches(sent, stored(TASK_CONTRACT_VERSION)),
        )
    }

    @Test
    fun `a server that predates versions is reported, not defaulted past`() {
        // 0 = the key was absent: an old server, or a field that failed to
        // persist. Defaulting past it is the 4.6.0 silent case.
        assertEquals(
            listOf("contract_version"),
            contractMismatches(sent, stored(0)),
        )
    }

    @Test
    fun `a server newer than the app is reported`() {
        // The app may misread fields the new contract added, so agreement on
        // every known field is not enough.
        assertEquals(
            listOf("contract_version"),
            contractMismatches(sent, stored(TASK_CONTRACT_VERSION + 1)),
        )
    }

    @Test
    fun `a version disagreement joins the field list once, not instead of it`() {
        // Both halves matter: the version says the contracts disagree, the
        // field says what visibly differs. One report, no duplicates.
        val got = contractMismatches(
            sent,
            stored(0).copy(dueDate = "2026-10-07"),
        )
        assertEquals(listOf("contract_version", "due_date"), got)
    }

    @Test
    fun `the expected version is a real version, not the absent default`() {
        // If TASK_CONTRACT_VERSION were ever 0, every pre-version response
        // would compare equal and the check would be dead. The constant must
        // stay positive for the 0-means-absent reading to mean anything.
        assertTrue(
            "TASK_CONTRACT_VERSION must be positive, got $TASK_CONTRACT_VERSION",
            TASK_CONTRACT_VERSION > 0,
        )
    }
}

/** Version parsing is lenient on type but strict on meaning (#185 review). */
class ParseContractVersionTest {

    @Test
    fun `numbers read as their int value`() {
        assertEquals(1, parseContractVersion(1))
        assertEquals(2, parseContractVersion(2L))
        assertEquals(3, parseContractVersion(3.0))
    }

    @Test
    fun `a numeric string reads as its number, not as pre-version`() {
        // A string "1" warning as an old server would be a false positive of
        // exactly the class this check exists to remove.
        assertEquals(1, parseContractVersion("1"))
        assertEquals(2, parseContractVersion("  2  "))
        // Double-encoded numbers get the same truncation Numbers do.
        assertEquals(1, parseContractVersion("1.0"))
    }

    @Test
    fun `absent null and garbage all mean predates versions`() {
        assertEquals(0, parseContractVersion(null))
        assertEquals(0, parseContractVersion("latest"))
        assertEquals(0, parseContractVersion(""))
        assertEquals(0, parseContractVersion(true))
    }

    @Test
    fun `negatives clamp to predates versions`() {
        // -5 fits neither bucket (0 = pre-version, >0 = real), so it must
        // not pass through as a version that can never equal
        // TASK_CONTRACT_VERSION yet reads as one.
        assertEquals(0, parseContractVersion(-5))
        assertEquals(0, parseContractVersion("-5"))
        assertEquals(0, parseContractVersion(-5.5))
    }
}

/** One number per response batch, not one prefs write per row (#185 review). */
class MaxContractVersionTest {

    private fun task(version: Int) = ServerTask(
        id = "6abf0000000000000000abcd",
        name = "Take meds",
        contractVersion = version,
    )

    @Test
    fun `the highest version in the batch wins`() {
        assertEquals(
            3,
            maxContractVersion(listOf(task(1), task(3), task(2))),
        )
    }

    @Test
    fun `an empty batch records nothing`() {
        assertEquals(0, maxContractVersion(emptyList()))
    }

    @Test
    fun `zeros are unknown, not a version`() {
        assertEquals(0, maxContractVersion(listOf(task(0), task(0))))
    }
}

/** A complete response is two docs, each asserted on its own (#185 review). */
class CompleteVersionMismatchTest {

    @Test
    fun `agreement on both halves warns nothing`() {
        assertFalse(completeVersionMismatch(TASK_CONTRACT_VERSION, TASK_CONTRACT_VERSION))
    }

    @Test
    fun `no next occurrence means nothing more to assert`() {
        // One-shots roll over to nothing: a null next is absence, not a
        // pre-version doc.
        assertFalse(completeVersionMismatch(TASK_CONTRACT_VERSION, null))
    }

    @Test
    fun `a pre-version next warns even when the task agrees`() {
        // The maxOf version of this collapsed task=1 + next=0 to 1 and hid
        // the half-disagreement — in both directions.
        assertTrue(completeVersionMismatch(TASK_CONTRACT_VERSION, 0))
    }

    @Test
    fun `a disagreeing task warns even when next agrees`() {
        assertTrue(completeVersionMismatch(0, TASK_CONTRACT_VERSION))
    }

    @Test
    fun `a newer server warns on either half`() {
        val newer = TASK_CONTRACT_VERSION + 1
        assertTrue(completeVersionMismatch(newer, TASK_CONTRACT_VERSION))
        assertTrue(completeVersionMismatch(TASK_CONTRACT_VERSION, newer))
    }
}
