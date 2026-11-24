package store_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

func TestSlugify(t *testing.T) {
	cases := []struct{ name, want string }{
		{"Fix login", "fix-login"},
		{"  Héllo, World!  ", "h-llo-world"},
		{"", "effort"},
		{"!!!", "effort"},
		// Cut at the last word boundary at or before 40 characters.
		{"v0.2 groundwork: highlighting, settings, conflicts", "v0-2-groundwork-highlighting-settings"},
		// A word ending exactly at 40 characters is kept whole.
		{strings.Repeat("a", 35) + " bcde fgh", strings.Repeat("a", 35) + "-bcde"},
		// One long word: no boundary late enough, so cut at 40.
		{strings.Repeat("x", 60), strings.Repeat("x", 40)},
		// The only boundary would leave a too-short slug: cut at 40.
		{"short " + strings.Repeat("y", 50), "short-" + strings.Repeat("y", 34)},
	}
	for _, c := range cases {
		got := store.Slugify(c.name)
		if got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.name, got, c.want)
		}
		if len(got) > 40 {
			t.Errorf("Slugify(%q) = %q is longer than 40", c.name, got)
		}
	}
}
