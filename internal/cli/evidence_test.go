package cli

import (
	"testing"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func TestGoTestCache(t *testing.T) {
	for _, tc := range []struct {
		name          string
		out           string
		cached, total int
		note          string
	}{
		{"none", "", 0, 0, ""},
		{"not go test", "all good\nok\n", 0, 0, ""},
		{"mixed", "ok  \tgithub.com/x/vx/internal/api\t(cached)\n" +
			"ok  \tgithub.com/x/vx/internal/cli\t1.204s\n" +
			"?   \tgithub.com/x/vx/cmd/vx\t[no test files]\n" +
			"ok  \tgithub.com/x/vx/internal/merge\t(cached)\n", 2, 3, "2 of 3 packages from the Go test cache"},
		{"all fresh", "ok  \texample.com/a\t0.012s\nok  \texample.com/b\t1.2s\n", 0, 2, ""},
		{"one cached", "ok  \texample.com/a\t(cached)\n", 1, 1, "1 of 1 package from the Go test cache"},
		{"coverage", "ok  \texample.com/a\t(cached)\tcoverage: 81.2% of statements\nok  \texample.com/b\t0.3s\tcoverage: 10.0% of statements\n", 1, 2, "1 of 2 packages from the Go test cache"},
		{"failure", "--- FAIL: TestX (0.00s)\n    x_test.go:9: boom\nFAIL\nFAIL\texample.com/a\t0.005s\nok  \texample.com/b\t(cached)\nFAIL\n", 1, 2, "1 of 2 packages from the Go test cache"},
		{"build failure", "FAIL\texample.com/a [build failed]\nok  \texample.com/b\t(cached)\r\n", 1, 2, "1 of 2 packages from the Go test cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached, total := goTestCache(tc.out)
			if cached != tc.cached || total != tc.total {
				t.Fatalf("goTestCache = %d of %d, want %d of %d", cached, total, tc.cached, tc.total)
			}
			if got := cacheNote(tc.out); got != tc.note {
				t.Fatalf("cacheNote = %q, want %q", got, tc.note)
			}
		})
	}
}

func TestSplitNewTests(t *testing.T) {
	got := splitNewTests([]string{"TestA,TestB", " TestC ", "TestA", ",", ""})
	want := []string{"TestA", "TestB", "TestC"}
	if len(got) != len(want) {
		t.Fatalf("splitNewTests = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitNewTests = %v", got)
		}
	}
}

func TestTestFuncRe(t *testing.T) {
	src := "package x\n\nfunc TestA(t *testing.T) {}\nfunc (s *suite) TestB() {}\nfunc helper() {}\nfunc FuzzC(f *testing.F) {}\n// func TestD(t *testing.T)\n"
	var names []string
	for _, m := range testFuncRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	if len(names) != 3 || names[0] != "TestA" || names[1] != "TestB" || names[2] != "FuzzC" {
		t.Fatalf("test funcs = %v", names)
	}
}

func TestNotesDropped(t *testing.T) {
	sent := &vxv1.Evidence{Checks: []*vxv1.Check{{Name: "go test ./...", Notes: "1 of 2 packages from the Go test cache"}}}
	if !notesDropped(sent, &vxv1.Evidence{Checks: []*vxv1.Check{{Name: "go test ./..."}}}) || !notesDropped(sent, nil) {
		t.Fatal("dropped notes not detected")
	}
	if notesDropped(sent, sent) || notesDropped(nil, nil) || notesDropped(&vxv1.Evidence{Checks: []*vxv1.Check{{Name: "x"}}}, nil) {
		t.Fatal("false alarm")
	}
}
