package hash

import (
	"errors"
	"testing"
)

func TestDomainsDiffer(t *testing.T) {
	data := []byte("same bytes")
	if Of(KindBlob, data) == Of(KindTree, data) {
		t.Fatal("blob and tree hashes of the same bytes must differ")
	}
	if Of(KindBlob, data) != Of(KindBlob, data) {
		t.Fatal("hash must be deterministic")
	}
}

func TestRoundTrip(t *testing.T) {
	h := Of(KindBlob, []byte("hello"))
	s := h.String()
	if len(s) != len(Prefix)+64 {
		t.Fatalf("unexpected length %d", len(s))
	}
	got, err := Parse(s)
	if err != nil || got != h {
		t.Fatalf("Parse(%q) = %v, %v", s, got, err)
	}
	got, err = Parse(h.Hex())
	if err != nil || got != h {
		t.Fatalf("Parse without prefix failed: %v", err)
	}
	b, _ := h.MarshalText()
	var u Hash
	if err := u.UnmarshalText(b); err != nil || u != h {
		t.Fatal("text round trip failed")
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"b3:abc", "b3:" + string(make([]byte, 64))} {
		if _, err := Parse(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
	if h, err := Parse(""); err != nil || !h.IsZero() {
		t.Error("empty string should parse to Zero")
	}
}

func TestScanValue(t *testing.T) {
	h := Of(KindChangeset, []byte("x"))
	v, _ := h.Value()
	var got Hash
	if err := got.Scan(v); err != nil || got != h {
		t.Fatal("scan round trip failed")
	}
	if v, _ := Zero.Value(); v != nil {
		t.Fatal("zero should be NULL")
	}
	if err := got.Scan(nil); err != nil || !got.IsZero() {
		t.Fatal("NULL should scan to Zero")
	}
}
