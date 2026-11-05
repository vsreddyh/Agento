package reports

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The vendored KaTeX must be exactly the pinned build: a silent upstream
// change ships different math under the same MathEngine label.
func TestKaTeXPin(t *testing.T) {
	raw, err := os.ReadFile("katex.min.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(raw))
	if sum != KaTeXSHA256 {
		t.Fatalf("katex.min.js sha256 = %s, want %s", sum, KaTeXSHA256)
	}
	if !strings.Contains(string(raw[:400]), "katex") {
		t.Fatal("katex.min.js does not look like KaTeX")
	}
}

func TestSegmentsPlain(t *testing.T) {
	for _, body := range []string{
		"Just prose, no math. Costs $5 and $10.",
		"Price $ 5$ with space.",
		"Trailing $x $ space.",
		"Dollars $$ $$ are spacing.",
	} {
		segs := Segments(body)
		if len(segs) != 1 || segs[0].Kind != "text" {
			t.Fatalf("%q: got %+v", body, segs)
		}
	}
}

func TestSegmentsInlineAndDisplay(t *testing.T) {
	segs := Segments(`The value $x^2$ matters. And: $$\sum_{i=1}^n i$$ done.`)
	var kinds []string
	for _, s := range segs {
		kinds = append(kinds, s.Kind)
	}
	want := []string{"text", "math", "text", "math", "text"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if !segs[1].Display && !strings.Contains(segs[1].HTML, "katex") {
		t.Fatalf("inline not rendered: %+v", segs[1])
	}
	if !segs[3].Display || !strings.Contains(segs[3].HTML, "katex-display") {
		t.Fatalf("display not rendered: %+v", segs[3])
	}
	if segs[1].Tex != "x^2" {
		t.Fatalf("tex = %q", segs[1].Tex)
	}
	// Reassembly: text + raw spans reconstruct the body.
	var sb strings.Builder
	for _, s := range segs {
		if s.Kind == "text" {
			sb.WriteString(s.Text)
		} else if s.Display {
			sb.WriteString("$$" + s.Tex + "$$")
		} else {
			sb.WriteString("$" + s.Tex + "$")
		}
	}
	if sb.String() != `The value $x^2$ matters. And: $$\sum_{i=1}^n i$$ done.` {
		t.Fatalf("reassembly = %q", sb.String())
	}
}

func TestSegmentsCodeIsLiteral(t *testing.T) {
	segs := Segments("Run `$x$` and:\n```tex\n$$y$$\n```\nthen $z$ ok.")
	maths := 0
	for _, s := range segs {
		if s.Kind == "math" {
			maths++
			if s.Tex != "z" {
				t.Fatalf("code math leaked: %+v", s)
			}
		}
	}
	if maths != 1 {
		t.Fatalf("maths = %d, want 1 (%+v)", maths, segs)
	}
}

func TestSegmentsEscapesAndUnclosed(t *testing.T) {
	// Escaped \$ is literal; unclosed $ is literal and swallows nothing.
	segs := Segments(`Price \$5 and $unclosed math here`)
	for _, s := range segs {
		if s.Kind == "math" {
			t.Fatalf("should be no math: %+v", segs)
		}
	}
	if len(segs) != 1 {
		t.Fatalf("got %+v", segs)
	}
}

func TestSegmentsInvalidTexSurvives(t *testing.T) {
	// throwOnError:false turns bad TeX into an error span, not a failure.
	segs := Segments(`Broken $\broken{$ here.`)
	found := false
	for _, s := range segs {
		if s.Kind == "math" {
			found = true
			if !strings.Contains(s.HTML, "katex") {
				t.Fatalf("no katex html: %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("invalid tex should still segment as math: %+v", segs)
	}
}

func TestHasMath(t *testing.T) {
	if hasMath("no dollars here") {
		t.Fatal("false positive")
	}
	if !hasMath("has $x$ math") {
		t.Fatal("false negative")
	}
	if hasMath("`$x$` in code only") {
		t.Fatal("code span counted as math")
	}
}
