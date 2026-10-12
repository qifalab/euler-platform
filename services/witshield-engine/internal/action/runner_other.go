// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

//go:build !linux && !darwin

package action

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}
