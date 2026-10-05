package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
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
     * Two back-to-back reports of the SAME fields must both reach a real collector.
     *
     * **No `consume` between them** — that is the whole point, and the previous version of
     * this test got it wrong. It did `report -> consume -> report`, so the second report
     * set against `null` rather than against `[X]`, and passed even with the buggy bare
     * `List<String>` payload it was written to guard. It asserted a thing that was never
     * at risk.
     *
     * The real failure is `StateFlow`'s distinct-until-changed: with a bare list,
     * `report([X])` twice sets an EQUAL value the second time, the flow conflates it, and
     * the collector never wakes. Nothing in the state changes to notice — the stored value
     * looks exactly right while the user is never told.
     *
     * So: report twice, assert the collector was invoked twice, and only then consume.
     */
    @Test
    fun `two back-to-back identical reports both reach a collector`() = runBlocking {
        val seen = mutableListOf<List<String>>()
        val job = launch(Dispatchers.Unconfined) {
            ContractWarnings.mismatched.collect { m -> if (m != null) seen += m.fields }
        }
        try {
            ContractWarnings.report(listOf("due_time"))
            ContractWarnings.report(listOf("due_time"))
        } finally {
            job.cancel()
        }
        assertEquals(
            "the second identical report was conflated away — StateFlow suppresses an " +
                "EQUAL value, so the payload must carry the generation to differ",
            listOf(listOf("due_time"), listOf("due_time")),
            seen,
        )
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
