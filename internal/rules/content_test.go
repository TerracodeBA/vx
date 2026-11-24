package rules

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

const landProto = `syntax = "proto3";
package vx.v1;

message LandRequest {
  string workspace_id = 1;
  repeated string runs = 2; // checks
}

message LandResponse {
  bool landed = 1;
  map<string, string> notes = 2;
  message Inner {
    int32 n = 1;
  }
  oneof result {
    string hash = 3;
    string error = 4;
  }
  optional string why = 5;
  // field 11 = 11 in a comment does not count
}
`

// The rule from vx's own vx.toml.
func protoFieldRule(t *testing.T) Unique {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := r.UniqueRule("proto-field")
	if !ok || !u.IsContent() || u.Glob != "proto/**/*.proto" {
		t.Fatalf("proto-field rule: %+v", r.Unique)
	}
	return u
}

func TestContentKeysProtoScopes(t *testing.T) {
	u := protoFieldRule(t)
	got := ContentKeys(regexp.MustCompile(u.Content), regexp.MustCompile(u.Scope), []byte(landProto))
	want := map[string][]int{
		"LandRequest 1": {5}, "LandRequest 2": {6},
		"LandResponse 1": {10}, "LandResponse 2": {11}, "LandResponse.Inner 1": {13},
		"LandResponse 3": {16}, "LandResponse 4": {17}, "LandResponse 5": {19},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v\nwant %v", got, want)
	}
	// Without braces a scope lasts until the next one starts.
	got = ContentKeys(regexp.MustCompile(`^(\w+) = (\d+)$`), regexp.MustCompile(`^\[(\w+)\]$`), []byte("[a]\nx = 1\n[b]\ny = 1\n"))
	if !reflect.DeepEqual(got, map[string][]int{"a x": {2}, "b y": {4}}) {
		t.Fatalf("brace-less keys = %v", got)
	}
}

// Two efforts both add field 12 to message LandResponse: each lands
// cleanly, the combined file uses 12 twice, and that is a rule violation
// naming both lines. Field 12 in another message is fine.
func TestCheckContentRuleProtoField(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	u := protoFieldRule(t)
	r := Rules{Unique: []Unique{u}}
	files := map[string]string{"vx.toml": "", "proto/vx/v1/vx.proto": landProto}
	base := tree(t, s, files)
	if ws, err := Check(ctx, s, r, hash.Hash{}, base); err != nil || len(ws) != 0 {
		t.Fatalf("the base has no clash: %+v %v", ws, err)
	}

	oneSide := strings.Replace(landProto, "  optional string why = 5;\n", "  optional string why = 5;\n  string shipped = 12;\n", 1)
	oneSide = strings.Replace(oneSide, "  repeated string runs = 2; // checks\n", "  repeated string runs = 2; // checks\n  bool quiet = 12;\n", 1)
	files["proto/vx/v1/vx.proto"] = oneSide
	one := tree(t, s, files)
	if ws, err := Check(ctx, s, r, base, one); err != nil || len(ws) != 0 {
		t.Fatalf("one effort's field 12 is fine: %+v %v", ws, err)
	}

	both := strings.Replace(oneSide, "  bool landed = 1;\n", "  bool landed = 1;\n  string forecast = 12;\n", 1)
	files["proto/vx/v1/vx.proto"] = both
	combined := tree(t, s, files)
	ws, err := Check(ctx, s, r, base, combined)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("warnings: %+v", ws)
	}
	w := ws[0]
	if w.Kind != WarningRule || w.Name != "proto-field" || w.Decl != "LandResponse 12" || w.Path != "proto/vx/v1/vx.proto" ||
		!reflect.DeepEqual(w.Files, []string{"proto/vx/v1/vx.proto"}) {
		t.Fatalf("warning: %+v", w)
	}
	if want := "proto-field 12 in LandResponse is used twice: proto/vx/v1/vx.proto lines 12 and 22 (rule proto-field in vx.toml)"; w.Detail != want {
		t.Fatalf("detail:\n%s\nwant\n%s", w.Detail, want)
	}

	// A clash that was there before the change is not repeated.
	files["README.md"] = "x"
	files["proto/vx/v1/vx.proto"] = both + "\nmessage Other {\n  string a = 1;\n}\n"
	later := tree(t, s, files)
	if ws, err := Check(ctx, s, r, combined, later); err != nil || len(ws) != 0 {
		t.Fatalf("old clash repeated: %+v %v", ws, err)
	}
}

// vx's own proto files follow the rule.
func TestContentRuleRepoProto(t *testing.T) {
	u := protoFieldRule(t)
	paths, err := filepath.Glob(filepath.Join("..", "..", "proto", "*", "*", "*.proto"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("proto files: %v %v", paths, err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		keys := ContentKeys(regexp.MustCompile(u.Content), regexp.MustCompile(u.Scope), data)
		if len(keys) < 50 {
			t.Fatalf("%s: only %d fields found", p, len(keys))
		}
		for k, lines := range keys {
			if len(lines) > 1 {
				t.Errorf("%s: %s is used on lines %v", p, k, lines)
			}
			if !strings.Contains(k, " ") {
				t.Errorf("%s: field %s on lines %v is outside every message", p, k, lines)
			}
		}
	}
}

func TestParseContentRules(t *testing.T) {
	r, err := Parse([]byte("[[unique]]\nname = \"code\"\nglob = \"errors/*.go\"\ncontent = 'Code\\((\\d+)\\)'\n"))
	if err != nil || len(r.Unique) != 1 || r.Unique[0].Content != `Code\((\d+)\)` || r.Unique[0].Key != "" {
		t.Fatalf("content without scope: %+v %v", r, err)
	}
	for src, want := range map[string]string{
		"[[unique]]\nname = \"m\"\nglob = \"a/*\"\nkey = '^(\\d+)'\ncontent = '(\\d+)'\n":      "not both",
		"[[unique]]\nname = \"m\"\nglob = \"a/*\"\nkey = '^(\\d+)'\nscope = '(\\w+)'\n":        "scope only applies with content",
		"[[unique]]\nname = \"m\"\nglob = \"a/*\"\ncontent = '\\d+'\n":                         `content "\\d+" has no group`,
		"[[unique]]\nname = \"m\"\nglob = \"a/*\"\ncontent = '(\\d+)'\nscope = 'message'\n":    `scope "message" has no group: put the part that names the scope`,
		"[[unique]]\nname = \"m\"\nglob = \"a/*\"\ncontent = '(\\d+)'\nscope = 'message (('\n": "is not a valid regular expression",
	} {
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", src, err, want)
		}
	}
}
