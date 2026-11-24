// Package highlight colours source code for the web UI's diffs. It
// tokenises a whole file with chroma, so strings and comments that span
// lines colour correctly, and hands back each line's tokens tagged with one
// of a small fixed set of CSS classes (tk-k, tk-s, ...). It never produces
// HTML: the templates render the tokens and escape their text.
package highlight

import (
	"bytes"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// MaxSize is the largest file that is highlighted; bigger files are shown
// plain to keep pages fast.
const MaxSize = 256 << 10

// Token is a run of text on one line with its CSS class ("" for plain).
type Token struct {
	Text  string
	Class string
}

// Span is a piece of a rendered diff line: syntax class plus whether it is
// part of the changed region of an edited line.
type Span struct {
	Text  string
	Class string
	Hi    bool
}

// Classes is the value of the span's class attribute ("" when the span
// needs no element at all).
func (s Span) Classes() string {
	switch {
	case s.Hi && s.Class != "":
		return s.Class + " hi"
	case s.Hi:
		return "hi"
	}
	return s.Class
}

var (
	mu      sync.Mutex
	byName  = map[string]chroma.Lexer{}
	maxKeys = 4096
)

// lexerFor finds the lexer for a file name, remembering the answer: chroma
// matches names against every lexer's globs, which is too slow to repeat
// for every file of every page. Lexers themselves compile their rules once.
func lexerFor(name string) chroma.Lexer {
	base := path.Base(name)
	mu.Lock()
	defer mu.Unlock()
	if l, ok := byName[base]; ok {
		return l
	}
	l := lexers.Match(base)
	if l != nil && l.Config().Name == "plaintext" {
		l = nil
	}
	if l != nil {
		l = chroma.Coalesce(l)
	}
	if len(byName) >= maxKeys {
		clear(byName)
	}
	byName[base] = l
	return l
}

// Budget bounds the time spent tokenising one file. Lines the lexer has not
// reached by then are shown plain.
const Budget = 150 * time.Millisecond

// File tokenises src, the contents of the file at name, and returns its
// tokens line by line: index 0 is line 1, and no token holds a newline.
// Tokenising stops once the first upto lines are done (all lines when upto
// is 0 or less), since a diff rarely shows the end of a file; lexing from
// the top still gets multi-line strings and comments right. The result can
// be shorter than the file; missing lines are meant to be shown plain.
// It returns nil when the file should be shown plain: no lexer knows the
// name, the file is larger than MaxSize, or it is binary or not UTF-8.
func File(name string, src []byte, upto int) (lines [][]Token) {
	if len(src) == 0 || len(src) > MaxSize || bytes.IndexByte(src, 0) >= 0 || !utf8.Valid(src) {
		return nil
	}
	l := lexerFor(name)
	if l == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			lines = nil
		}
	}()
	it, err := l.Tokenise(&chroma.TokeniseOptions{State: "root"}, string(src))
	if err != nil {
		return nil
	}
	return splitLines(it, upto, time.Now().Add(Budget))
}

// splitLines cuts a token stream at newlines, merging neighbours of the
// same class so lines carry few spans. It stops after upto complete lines
// (when upto > 0) or at the deadline, dropping the unfinished line.
func splitLines(it chroma.Iterator, upto int, deadline time.Time) [][]Token {
	lines := [][]Token{nil}
	add := func(text, class string) {
		if text == "" {
			return
		}
		cur := &lines[len(lines)-1]
		if n := len(*cur); n > 0 && (*cur)[n-1].Class == class {
			(*cur)[n-1].Text += text
			return
		}
		*cur = append(*cur, Token{Text: text, Class: class})
	}
	for n := 1; ; n++ {
		if upto > 0 && len(lines) > upto {
			return lines[:upto]
		}
		if n%512 == 0 && time.Now().After(deadline) {
			return lines[:len(lines)-1]
		}
		tok := it()
		if tok == chroma.EOF {
			break
		}
		class := Class(tok.Type)
		text := tok.Value
		for {
			i := strings.IndexByte(text, '\n')
			if i < 0 {
				add(text, class)
				break
			}
			add(text[:i], class)
			lines = append(lines, nil)
			text = text[i+1:]
		}
	}
	return lines
}

// Class maps a chroma token type to one of the UI's syntax classes.
func Class(t chroma.TokenType) string {
	switch t {
	case chroma.KeywordType, chroma.NameBuiltin, chroma.NameBuiltinPseudo, chroma.NameClass, chroma.NameException:
		return "tk-t"
	case chroma.KeywordConstant, chroma.NameConstant:
		return "tk-n"
	case chroma.CommentPreproc, chroma.CommentPreprocFile:
		return "tk-k"
	case chroma.NameFunction, chroma.NameFunctionMagic:
		return "tk-f"
	case chroma.NameTag:
		return "tk-tag"
	case chroma.NameAttribute, chroma.NameDecorator, chroma.NameVariable, chroma.NameVariableClass,
		chroma.NameVariableGlobal, chroma.NameVariableInstance, chroma.NameVariableMagic, chroma.NameVariableAnonymous:
		return "tk-a"
	case chroma.GenericHeading, chroma.GenericSubheading:
		return "tk-k"
	case chroma.GenericInserted:
		return "tk-tag"
	case chroma.GenericDeleted:
		return "tk-k"
	}
	switch {
	case t.InCategory(chroma.Keyword):
		return "tk-k"
	case t.InCategory(chroma.Comment):
		return "tk-c"
	case t.InSubCategory(chroma.LiteralNumber):
		return "tk-n"
	case t.InCategory(chroma.Literal):
		return "tk-s"
	case t.InCategory(chroma.Operator):
		return "tk-o"
	case t.InCategory(chroma.Punctuation):
		return "tk-p"
	}
	return ""
}

// Overlay combines a line's syntax tokens with its word-level change
// marks (segs; nil when the line was not edited in place) into spans cut
// at both kinds of boundary. text is the line as the diff shows it; when
// the tokens do not spell it exactly, Overlay returns nil and the caller
// shows the line without syntax colours.
func Overlay(toks []Token, text string, segs []Span) []Span {
	n := 0
	for _, t := range toks {
		if n+len(t.Text) > len(text) || text[n:n+len(t.Text)] != t.Text {
			return nil
		}
		n += len(t.Text)
	}
	if n != len(text) {
		return nil
	}
	if len(segs) == 0 {
		out := make([]Span, 0, len(toks))
		for _, t := range toks {
			out = append(out, Span{Text: t.Text, Class: t.Class})
		}
		return out
	}
	var out []Span
	add := func(s Span) {
		if k := len(out); k > 0 && out[k-1].Class == s.Class && out[k-1].Hi == s.Hi {
			out[k-1].Text += s.Text
			return
		}
		out = append(out, s)
	}
	ti, si, toff, soff := 0, 0, 0, 0
	for ti < len(toks) && si < len(segs) {
		t, s := toks[ti].Text[toff:], segs[si].Text[soff:]
		k := min(len(t), len(s))
		if k > 0 {
			add(Span{Text: t[:k], Class: toks[ti].Class, Hi: segs[si].Hi})
		}
		toff += k
		soff += k
		if toff == len(toks[ti].Text) {
			ti, toff = ti+1, 0
		}
		if soff == len(segs[si].Text) {
			si, soff = si+1, 0
		}
	}
	if ti < len(toks) || joined(segs) != text {
		return nil
	}
	return out
}

func joined(segs []Span) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}
