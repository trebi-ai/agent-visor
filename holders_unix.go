//go:build !windows

package agentvisor

import (
	"os"
	"syscall"
)

// ownedByUser tells whether this user owns the file.
func ownedByUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// detached gives a new process session, so that the holder outlives the visor.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
