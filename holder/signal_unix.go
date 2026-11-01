//go:build !windows

package holder

import "syscall"

// signal sends sig to the process group. The group can be gone already.
func (s *server) signal(sig syscall.Signal) { _ = syscall.Kill(-s.cmd.Process.Pid, sig) }
