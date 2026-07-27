//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

import (
	"os"
	"os/exec"
)

// Non-Unix targets do not expose POSIX process groups through syscall.
// Closing stdout in stream cleanup still prevents descendants from holding the
// iterator open; this fallback terminates the direct process.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return terminateProcessTree(cmd)
	}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
