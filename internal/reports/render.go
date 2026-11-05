// Server-side math for ∃-Mail reports (#365): LaTeX compiles on the server,
// not on the client.
//
// The app ships no math library (mikepenz markdown only). Instead the server
// vendors KaTeX (katex.min.js, MIT) and runs it inside pure-Go goja — no node,
// no system dependency, so it works identically in the health-api alpine image
// and the pi image, and stays offline: the API returns pre-rendered HTML the
// app displays verbatim. What the app bundles is static display CSS, never a
// compiler.
//
// KaTeX with throwOnError:false never throws on bad TeX (it returns an error
// span), so a broken formula degrades to a visible marker, never a failed read.
package reports

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/dop251/goja"
)

//go:embed katex.min.js
var katexSrc string

// MathEngine identifies the renderer for API responses, so the app knows which
// static CSS the pre-rendered HTML expects.
const MathEngine = "katex 0.16.21"

// KaTeXSHA256 is the pinned digest of the vendored katex.min.js. A silent
// upstream change must fail loudly, not ship different math.
const KaTeXSHA256 = "863811e2baa0849c77bc92d26d44f4d0a4843c2a8ab52c462017dea57316e4d8"

const (
	// maxMathFrags caps rendered fragments per body: each is a goja call, and
	// a body with hundreds of formulas is either generated or hostile.
	maxMathFrags = 50
	// maxTexLen caps one fragment: KaTeX on megabytes of TeX is a DoS-shaped
	// input, and a real formula never approaches this.
	maxTexLen = 2000
)

// Segment is one piece of a rendered body. Kind is "text" (markdown, rendered
// by the client's mikepenz) or "math" (pre-rendered HTML, displayed verbatim).
type Segment struct {
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Tex     string `json:"tex,omitempty"`
	HTML    string `json:"html,omitempty"`
	Display bool   `json:"display,omitempty"`
}

// frag is one $..$ / $$..$$ span: byte offsets of the whole span in body.
type frag struct {
	display    bool
	tex        string
	start, end int
}

var (
	rtMu sync.Mutex
	rt   *goja.Runtime
	rtOK bool
)

// renderInit loads KaTeX into the shared runtime once. goja Runtimes are not
// goroutine-safe, so every use holds rtMu; renders are read-path and
// infrequent, and one runtime (~100ms load, once per process) beats a pool.
func renderInit() error {
	if rtOK {
		return nil
	}
	r := goja.New()
	if _, err := r.RunString(katexSrc); err != nil {
		return fmt.Errorf("katex load: %w", err)
	}
	if _, err := r.RunString(`var __render = function(tex, display) { return katex.renderToString(tex, {throwOnError: false, displayMode: !!display}); };`); err != nil {
		return fmt.Errorf("katex shim: %w", err)
	}
	rt = r
	rtOK = true
	return nil
}

// renderTex compiles one fragment. It cannot throw on bad TeX
// (throwOnError:false returns an error span); the recover is for the runtime
// itself failing, in which case the fragment stays literal text.
func renderTex(tex string, display bool) (html string, ok bool) {
	rtMu.Lock()
	defer rtMu.Unlock()
	if err := renderInit(); err != nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			html, ok = "", false
		}
	}()
	fn, ok := goja.AssertFunction(rt.Get("__render"))
	if !ok {
		return "", false
	}
	v, err := fn(goja.Undefined(), rt.ToValue(tex), rt.ToValue(display))
	if err != nil {
		return "", false
	}
	return v.String(), true
}

// Segments splits a report body into text/math segments, rendering each math
// fragment server-side. Code spans and fenced blocks are NOT math: `$` inside
// backticks stays literal, so documenting a formula never compiles it.
// Escaped `\$` stays literal too. An unclosed `$` stays literal — the writer
// typo shows as typed, rather than swallowing the rest of the body as math.
func Segments(body string) []Segment {
	frags := scanFrags(body)
	if len(frags) == 0 {
		return []Segment{{Kind: "text", Text: body}}
	}
	segs := []Segment{}
	pos := 0
	for _, f := range frags {
		if f.start > pos {
			segs = append(segs, Segment{Kind: "text", Text: body[pos:f.start]})
		}
		html, ok := renderTex(f.tex, f.display)
		if !ok {
			// Renderer down: keep the source visible rather than dropping the
			// fragment. Offline-readable beats pretty.
			segs = append(segs, Segment{Kind: "text", Text: body[f.start:f.end]})
		} else {
			segs = append(segs, Segment{Kind: "math", Tex: f.tex, HTML: html, Display: f.display})
		}
		pos = f.end
	}
	if pos < len(body) {
		segs = append(segs, Segment{Kind: "text", Text: body[pos:]})
	}
	return segs
}

// hasMath reports whether a body contains at least one math fragment, so
// callers can skip rendering work for plain prose.
func hasMath(body string) bool { return len(scanFrags(body)) > 0 }

// scanFrags finds math spans without rendering. Shared rule: code is masked,
// `\$` is literal, unclosed and empty spans are literal, over-cap fragments
// are literal. Delimiter adjacency follows the common markdown-math
// convention: an opener must be followed by a non-space (so "costs $5" is
// currency, not math), and an inline closer must be preceded by a non-space
// (so "$5 and $10" never pairs across the words). Display `$$` closers stay
// lenient — block math is routinely written with newlines around it.
func scanFrags(body string) []frag {
	var frags []frag
	masked := maskCode(body)
	i := 0
	for i < len(masked) {
		if masked[i] == '\\' && i+1 < len(masked) && masked[i+1] == '$' {
			i += 2
			continue
		}
		if masked[i] != '$' {
			i++
			continue
		}
		display := i+1 < len(masked) && masked[i+1] == '$'
		delim := 1
		if display {
			delim = 2
		}
		if i+delim >= len(masked) || isMathSpace(masked[i+delim]) {
			i++
			continue
		}
		j := i + delim
		k := -1
		for j < len(masked) {
			if masked[j] == '\\' && j+1 < len(masked) && masked[j+1] == '$' {
				j += 2
				continue
			}
			if display {
				if masked[j] == '$' && j+1 < len(masked) && masked[j+1] == '$' {
					k = j
					break
				}
				j++
				continue
			}
			if masked[j] == '$' {
				// A single `$` closes inline math only when it is not part of
				// `$$`: `$$` mid-scan is a display delimiter, not an empty
				// inline close followed by an opener. It must also be preceded
				// by a non-space, or "$5 and $10" pairs across the words.
				if j+1 < len(masked) && masked[j+1] == '$' {
					j += 2
					continue
				}
				if isMathSpace(masked[j-1]) {
					j++
					continue
				}
				k = j
				break
			}
			j++
		}
		if k == -1 {
			i += delim
			continue
		}
		tex := body[i+delim : k]
		close := delim
		if display && k+1 < len(body) {
			close = 2
		}
		if tex == "" || len(tex) > maxTexLen {
			i = k + close
			continue
		}
		if strings.TrimSpace(tex) == "" {
			// "$$ $$" is spacing, not math.
			i = k + close
			continue
		}
		frags = append(frags, frag{display: display, tex: tex, start: i, end: k + close})
		i = k + close
		if len(frags) >= maxMathFrags {
			break
		}
	}
	return frags
}

// isMathSpace reports the whitespace that disqualifies a `$` adjacency: an
// opener must be FOLLOWED by a non-space and an inline closer PRECEDED by one.
func isMathSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// maskCode blanks code spans and fenced blocks (same length, spaces) so the
// math scanner never sees a `$` inside backticks. Fences open with ``` (or
// more) and close with a run at least as long; inline spans work the same way.
func maskCode(s string) string {
	b := []byte(s)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(a, c int) {
		for k := a; k < c && k < len(out); k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	i := 0
	for i < len(b) {
		if b[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(b) && b[j] == '`' {
			j++
		}
		n := j - i
		if n >= 3 {
			// Fenced block: close at the next run of >= n backticks.
			k := j
			closeAt := -1
			closeEnd := -1
			for k < len(b) {
				if b[k] == '`' {
					m := k
					for m < len(b) && b[m] == '`' {
						m++
					}
					if m-k >= n {
						closeAt, closeEnd = k, m
						break
					}
					k = m
					continue
				}
				k++
			}
			if closeAt == -1 {
				blank(i, len(b))
				break
			}
			blank(i, closeEnd)
			i = closeEnd
			continue
		}
		// Inline span: close at the next run of exactly n backticks on the
		// same line. A newline before the close means unclosed — literal.
		k := j
		closeAt := -1
		for k < len(b) && b[k] != '\n' {
			if b[k] == '`' {
				m := k
				for m < len(b) && b[m] == '`' {
					m++
				}
				if m-k == n {
					closeAt = m
					break
				}
				k = m
				continue
			}
			k++
		}
		if closeAt == -1 {
			i = j
			continue
		}
		blank(i, closeAt)
		i = closeAt
	}
	return string(out)
}
