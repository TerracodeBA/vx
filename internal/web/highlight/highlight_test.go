package highlight

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func text(toks []Token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.Text)
	}
	return b.String()
}

func classOf(toks []Token, s string) string {
	for _, t := range toks {
		if strings.Contains(t.Text, s) {
			return t.Class
		}
	}
	return "missing"
}

func TestFileSplitsMultiLineComment(t *testing.T) {
	src := "package p\n\n/* one\ntwo\nthree */\nfunc F() string { return \"x\" } // end\n"
	lines := File("p/p.go", []byte(src), 0)
	want := strings.Split(src, "\n")
	if len(lines) < len(want)-1 {
		t.Fatalf("got %d lines, want %d", len(lines), len(want)-1)
	}
	for i, w := range want[:len(want)-1] {
		if got := text(lines[i]); got != w {
			t.Fatalf("line %d = %q, want %q", i+1, got, w)
		}
		for _, tok := range lines[i] {
			if strings.Contains(tok.Text, "\n") {
				t.Fatalf("line %d holds a newline: %q", i+1, tok.Text)
			}
		}
	}
	for i, l := range []int{3, 4, 5} {
		if len(lines[l-1]) != 1 || lines[l-1][0].Class != "tk-c" {
			t.Errorf("comment line %d (%d) = %+v", l, i, lines[l-1])
		}
	}
	last := lines[5]
	for s, want := range map[string]string{"func": "tk-k", "F": "tk-f", "string": "tk-t", `"x"`: "tk-s", "// end": "tk-c", "{": "tk-p"} {
		if got := classOf(last, s); got != want {
			t.Errorf("%q: class %q, want %q (%+v)", s, got, want, last)
		}
	}
	if got := classOf(lines[0], "package"); got != "tk-k" {
		t.Errorf("package: %q", got)
	}
}

func TestFileMultiLineString(t *testing.T) {
	src := "var s = `a\nb`\nvar n = 42\n"
	lines := File("x.go", []byte(src), 0)
	if classOf(lines[1], "b") != "tk-s" {
		t.Fatalf("second line of a raw string = %+v", lines[1])
	}
	if classOf(lines[2], "42") != "tk-n" {
		t.Fatalf("number = %+v", lines[2])
	}
}

func TestFileStopsAtUpto(t *testing.T) {
	src := []byte("/* a\nb */\nvar x = 1\nvar y = 2\n")
	lines := File("x.go", src, 2)
	if len(lines) != 2 || classOf(lines[1], "b */") != "tk-c" {
		t.Fatalf("upto 2 = %+v", lines)
	}
}

func TestSplitLinesDeadline(t *testing.T) {
	it, err := lexerFor("x.go").Tokenise(nil, strings.Repeat("var x = 1\n", 1000))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(it, 0, time.Now().Add(-time.Second))
	if len(lines) == 0 || len(lines) >= 1000 {
		t.Fatalf("past the deadline: %d lines", len(lines))
	}
	if got := text(lines[len(lines)-1]); got != "var x = 1" {
		t.Fatalf("last line kept is unfinished: %q", got)
	}
}

func TestFileKeepsCarriageReturns(t *testing.T) {
	lines := File("x.go", []byte("package p\r\nvar x = 1\r\n"), 0)
	if got := text(lines[0]); got != "package p\r" {
		t.Fatalf("line 1 = %q", got)
	}
}

func TestFilePlain(t *testing.T) {
	for name, src := range map[string]string{
		"notes.unknownext": "hello\n",
		"README.txt":       "hello\n",
		"bin.go":           "package p\x00\n",
		"bad.go":           "package p \xff\n",
		"big.go":           "package p\n" + strings.Repeat("// x\n", MaxSize/5+1),
		"empty.go":         "",
	} {
		if got := File(name, []byte(src), 0); got != nil {
			t.Errorf("%s: highlighted, want plain", name)
		}
	}
}

func TestLexerCached(t *testing.T) {
	a, b := lexerFor("a/b.go"), lexerFor("c/b.go")
	if a == nil || a != b {
		t.Fatal("lexer should be found and cached by base name")
	}
	if lexerFor("Makefile") == nil || lexerFor("x.py") == nil {
		t.Fatal("common files should have lexers")
	}
}

func TestOverlayMixesSegsAndTokens(t *testing.T) {
	toks := []Token{{"func", "tk-k"}, {" ", ""}, {"Login", "tk-f"}, {"(", "tk-p"}, {"ctx", ""}, {" ", ""}, {"Context", "tk-t"}, {")", "tk-p"}}
	line := "func Login(ctx Context)"
	segs := []Span{{Text: "func Login("}, {Text: "ctx Context", Hi: true}, {Text: ")"}}
	got := Overlay(toks, line, segs)
	want := []Span{
		{Text: "func", Class: "tk-k"}, {Text: " "}, {Text: "Login", Class: "tk-f"}, {Text: "(", Class: "tk-p"},
		{Text: "ctx ", Hi: true}, {Text: "Context", Class: "tk-t", Hi: true}, {Text: ")", Class: "tk-p"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay =\n%+v\nwant\n%+v", got, want)
	}
	if got[5].Classes() != "tk-t hi" || got[4].Classes() != "hi" || got[1].Classes() != "" {
		t.Fatalf("classes = %q %q %q", got[5].Classes(), got[4].Classes(), got[1].Classes())
	}
}

func TestOverlaySplitsTokenAcrossSegs(t *testing.T) {
	// The change starts in the middle of a string token.
	toks := []Token{{"x", ""}, {" = ", "tk-o"}, {`"hello"`, "tk-s"}}
	segs := []Span{{Text: `x = "he`}, {Text: "llo", Hi: true}, {Text: `"`}}
	got := Overlay(toks, `x = "hello"`, segs)
	want := []Span{{Text: "x"}, {Text: " = ", Class: "tk-o"}, {Text: `"he`, Class: "tk-s"}, {Text: "llo", Class: "tk-s", Hi: true}, {Text: `"`, Class: "tk-s"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay = %+v", got)
	}
}

func TestOverlayNoSegsAndMismatch(t *testing.T) {
	toks := []Token{{"a", "tk-k"}, {"b", ""}}
	if got := Overlay(toks, "ab", nil); !reflect.DeepEqual(got, []Span{{Text: "a", Class: "tk-k"}, {Text: "b"}}) {
		t.Fatalf("no segs = %+v", got)
	}
	if Overlay(toks, "abc", nil) != nil || Overlay(toks, "xb", nil) != nil || Overlay(toks, "ab", []Span{{Text: "a"}}) != nil {
		t.Fatal("tokens or segs that do not spell the line give nil")
	}
	if got := Overlay(nil, "", nil); len(got) != 0 {
		t.Fatalf("empty line = %+v", got)
	}
}

func BenchmarkFile(b *testing.B) {
	src := []byte(strings.Repeat("// Comment\nfunc F(a int) string {\n\treturn fmt.Sprintf(\"%d\", a+1)\n}\n", 500))
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		File("x.go", src, 0)
	}
}
