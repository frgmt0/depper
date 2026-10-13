//go:build !unix

package transport

import "os/exec"

// ownProcessGroup is a no-op where process groups are not available.
func ownProcessGroup(*exec.Cmd) {}
