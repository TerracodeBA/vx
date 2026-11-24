//go:build !unix

package cli

import "os/exec"

// ownProcessGroup is a no-op here: stopping c kills only its own process.
func ownProcessGroup(c *exec.Cmd) {}
