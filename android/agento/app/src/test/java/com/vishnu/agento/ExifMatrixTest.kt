package com.vishnu.agento

import android.media.ExifInterface
import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * 2x1 corner probe for the EXIF orientation matrices in
 * `prepareAttachment` (#339): the affine math — not just the prose —
 * must pin every case to positive coordinates with the EXIF-correct
 * corner mapping.
 *
 * Pure 3x3 replication on purpose: android.graphics is stubbed on the
 * JVM, so this mirrors the production op ORDER with the DOCUMENTED
 * platform identities — row-major [a b tx; c d ty; 0 0 1],
 * postFoo(op) = op * M per
 * developer.android.com/reference/android/graphics/Matrix (preFoo would be
 * M * op, op applied first) — and asserts the EXIF-semantics outcome
 * (CIPA DC-008: TRANSPOSE = rot90 + flip-H, TRANSVERSE = rot270 + flip-H)
 * on a W=2,H=1 probe. The replication source is the platform doc, not
 * the production call order: a shared order misunderstanding between the
 * two files would still have to survive the CIPA geometric assertions
 * below, which are derived from the EXIF spec, not from Matrix. If
 * production edits the op order, this test — not the device camera roll
 * — catches it. The framework's createBitmap normalization is deliberately
 * NOT modeled: the matrices must already be positive-mapped (asserted),
 * so normalization is never load-bearing.
 */
class ExifMatrixTest {

    private fun ident() = floatArrayOf(1f, 0f, 0f, 0f, 1f, 0f, 0f, 0f, 1f)

    private fun mul(a: FloatArray, b: FloatArray): FloatArray {
        val o = FloatArray(9)
        for (r in 0..2) for (c in 0..2) {
            var s = 0f
            for (k in 0..2) s += a[r * 3 + k] * b[k * 3 + c]
            o[r * 3 + c] = s
        }
        return o
    }

    private fun rot(deg: Float): FloatArray {
        val r = Math.toRadians(deg.toDouble())
        val c = Math.cos(r).toFloat()
        val s = Math.sin(r).toFloat()
        return floatArrayOf(c, -s, 0f, s, c, 0f, 0f, 0f, 1f)
    }

    private fun trans(tx: Float, ty: Float) =
        floatArrayOf(1f, 0f, tx, 0f, 1f, ty, 0f, 0f, 1f)

    private fun scale(sx: Float, sy: Float) =
        floatArrayOf(sx, 0f, 0f, 0f, sy, 0f, 0f, 0f, 1f)

    /** Mirror of the production op sequence for one orientation (W=2,H=1). */
    private fun matrixFor(orientation: Int): FloatArray {
        val w = 2f
        val h = 1f
        // postFoo left-multiplies — exactly the production post+post
        // pattern above: each call applies AFTER everything before it, so
        // both files read top-down as the pixel flows.
        fun post(m: FloatArray, op: FloatArray) = mul(op, m)
        var m = ident()
        when (orientation) {
            ExifInterface.ORIENTATION_ROTATE_90 -> {
                m = post(m, rot(90f)); m = post(m, trans(h, 0f))
            }
            ExifInterface.ORIENTATION_ROTATE_180 -> {
                m = post(m, rot(180f)); m = post(m, trans(w, h))
            }
            ExifInterface.ORIENTATION_ROTATE_270 -> {
                m = post(m, rot(270f)); m = post(m, trans(0f, w))
            }
            ExifInterface.ORIENTATION_FLIP_HORIZONTAL -> {
                m = post(m, scale(-1f, 1f)); m = post(m, trans(w, 0f))
            }
            ExifInterface.ORIENTATION_FLIP_VERTICAL -> {
                m = post(m, scale(1f, -1f)); m = post(m, trans(0f, h))
            }
            ExifInterface.ORIENTATION_TRANSPOSE -> {
                m = post(m, rot(90f)); m = post(m, trans(h, 0f))
                m = post(m, scale(-1f, 1f)); m = post(m, trans(h, 0f))
            }
            ExifInterface.ORIENTATION_TRANSVERSE -> {
                m = post(m, rot(270f)); m = post(m, trans(0f, w))
                m = post(m, scale(-1f, 1f)); m = post(m, trans(h, 0f))
            }
            else -> Unit
        }
        return m
    }

    private fun at(m: FloatArray, x: Float, y: Float): Pair<Float, Float> =
        (m[0] * x + m[1] * y + m[2]) to (m[3] * x + m[4] * y + m[5])

    private fun assertMaps(
        orientation: Int,
        outW: Float,
        outH: Float,
        c00: Pair<Float, Float>,
        cW0: Pair<Float, Float>,
        c0H: Pair<Float, Float>,
    ) {
        val m = matrixFor(orientation)
        val pts = listOf(at(m, 0f, 0f), at(m, 2f, 0f), at(m, 0f, 1f))
        // Positive-mapped: no corner may leave the output box.
        for ((px, py) in pts) {
            assertEquals(true, px >= -1e-5f && px <= outW + 1e-5f)
            assertEquals(true, py >= -1e-5f && py <= outH + 1e-5f)
        }
        val exp = listOf(c00, cW0, c0H)
        pts.zip(exp).forEach { (got, want) ->
            assertEquals(want.first, got.first, 1e-5f)
            assertEquals(want.second, got.second, 1e-5f)
        }
    }

    @Test
    fun rotationsMapPositive() {
        // CW in screen coords (y down): rot90 (x,y) -> (H-y,x).
        assertMaps(ExifInterface.ORIENTATION_ROTATE_90, 1f, 2f,
            1f to 0f, 1f to 2f, 0f to 0f)
        assertMaps(ExifInterface.ORIENTATION_ROTATE_180, 2f, 1f,
            2f to 1f, 0f to 1f, 2f to 0f)
        assertMaps(ExifInterface.ORIENTATION_ROTATE_270, 1f, 2f,
            0f to 2f, 0f to 0f, 1f to 2f)
    }

    @Test
    fun flipsMapPositive() {
        assertMaps(ExifInterface.ORIENTATION_FLIP_HORIZONTAL, 2f, 1f,
            2f to 0f, 0f to 0f, 2f to 1f)
        assertMaps(ExifInterface.ORIENTATION_FLIP_VERTICAL, 2f, 1f,
            0f to 1f, 2f to 1f, 0f to 0f)
    }

    @Test
    fun transposeAndTransverseMapPositive() {
        // Transpose (x,y) -> (y,x); transverse (x,y) -> (H-y,W-x).
        assertMaps(ExifInterface.ORIENTATION_TRANSPOSE, 1f, 2f,
            0f to 0f, 0f to 2f, 1f to 0f)
        assertMaps(ExifInterface.ORIENTATION_TRANSVERSE, 1f, 2f,
            1f to 2f, 1f to 0f, 0f to 2f)
    }
}
