package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Files tree flattening (#583): visible rows, nesting depth, and
 * expand/collapse membership — all pure, no server involved.
 */
class StorageTreeTest {

    private fun entry(name: String, size: Long = 10L) =
        RemoteEntry(name, false, size)

    @Test
    fun `empty tree flattens to nothing`() {
        assertEquals(emptyList<TreeRow>(), flattenTree(emptyMap(), emptySet()))
    }

    @Test
    fun `closed folders list dirs then files at depth zero`() {
        val tree = mapOf(
            "" to DirListing(
                listOf(RemoteEntry("b", true), RemoteEntry("a", true)),
                listOf(entry("z.txt"), entry("y.txt")),
            ),
        )
        assertEquals(
            listOf(
                TreeRow.Dir("b", "b", 0),
                TreeRow.Dir("a", "a", 0),
                TreeRow.File("", entry("z.txt"), 0),
                TreeRow.File("", entry("y.txt"), 0),
            ),
            flattenTree(tree, emptySet()),
        )
    }

    @Test
    fun `open folder nests its rows one level deeper`() {
        val tree = mapOf(
            "" to DirListing(listOf(RemoteEntry("sub", true)), emptyList()),
            "sub" to DirListing(emptyList(), listOf(entry("inner.txt"))),
        )
        assertEquals(
            listOf(
                TreeRow.Dir("sub", "sub", 0),
                TreeRow.File("sub", entry("inner.txt"), 1),
            ),
            flattenTree(tree, setOf("sub")),
        )
    }

    @Test
    fun `an open folder without a cached listing contributes no rows`() {
        val tree = mapOf(
            "" to DirListing(listOf(RemoteEntry("sub", true)), emptyList()),
        )
        assertEquals(
            listOf(TreeRow.Dir("sub", "sub", 0)),
            flattenTree(tree, setOf("sub", "ghost")),
        )
    }

    @Test
    fun `an open empty folder draws one empty hint instead of nothing`() {
        val tree = mapOf(
            "" to DirListing(listOf(RemoteEntry("sub", true)), emptyList()),
            "sub" to DirListing(emptyList(), emptyList()),
        )
        assertEquals(
            listOf(
                TreeRow.Dir("sub", "sub", 0),
                TreeRow.Empty("sub", 1),
            ),
            flattenTree(tree, setOf("sub")),
        )
    }

    @Test
    fun `a closed empty folder draws no hint`() {
        val tree = mapOf(
            "" to DirListing(listOf(RemoteEntry("sub", true)), emptyList()),
            "sub" to DirListing(emptyList(), emptyList()),
        )
        assertEquals(
            listOf(TreeRow.Dir("sub", "sub", 0)),
            flattenTree(tree, emptySet()),
        )
    }

    @Test
    fun `nesting composes to arbitrary depth`() {
        val tree = mapOf(
            "" to DirListing(listOf(RemoteEntry("a", true)), emptyList()),
            "a" to DirListing(listOf(RemoteEntry("b", true)), emptyList()),
            "a/b" to DirListing(emptyList(), listOf(entry("deep.txt"))),
        )
        assertEquals(
            listOf(
                TreeRow.Dir("a", "a", 0),
                TreeRow.Dir("a/b", "b", 1),
                TreeRow.File("a/b", entry("deep.txt"), 2),
            ),
            flattenTree(tree, setOf("a", "a/b")),
        )
    }
}
