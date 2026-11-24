package service

import "testing"

func TestTargetsMatch(t *testing.T) {
	for _, tc := range []struct {
		target, want string
		ok           bool
	}{
		{"internal/http/limit.go", "internal/http/limit.go", true},
		{"internal/http/", "internal/http/limit.go", true},
		{"internal/http", "internal/http/limit.go", true},
		{"internal/http/limit.go", "internal/http", true},
		{"auth.Login", "auth", true},
		{"auth", "auth.Login", true},
		{"internal/httpapi/x.go", "internal/http", false},
		{"auth.LoginFlow", "auth.Login", false},
		{"a", "b", false},
		{"auth", "auth.go", false},
		{"auth.go", "auth", false},
		{"auth.Login", "auth.go", false},
		{"README", "README.md", false},
		{"auth.Login", "auth.Login.Validate", true},
		{"web", "web.go", false},
	} {
		if got := TargetsMatch(tc.target, tc.want); got != tc.ok {
			t.Errorf("TargetsMatch(%q, %q) = %v", tc.target, tc.want, got)
		}
	}
}
