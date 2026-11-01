package agentvisor

import (
	"os"
	"syscall"
)

// ownedByUser is true: Windows has no file owner in the file mode.
func ownedByUser(os.FileInfo) bool { return true }

// detached gives a new process group, so that the holder outlives the visor.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
