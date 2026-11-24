package cli

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// vx status stays quiet about tasks on servers that cannot do them: older
// servers (404, Unimplemented) and the contract's stubs.
func TestNoTaskService(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{connect.NewError(connect.CodeUnimplemented, errors.New("404 Not Found")), true},
		{connect.NewError(connect.CodeUnavailable, errors.New("claims and tasks are not implemented yet")), true},
		{connect.NewError(connect.CodeUnavailable, errors.New("database down")), false},
		{connect.NewError(connect.CodeNotFound, errors.New("no effort x")), false},
		{errors.New("dial tcp: refused"), false},
	} {
		if got := noTaskService(tc.err); got != tc.want {
			t.Errorf("noTaskService(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
