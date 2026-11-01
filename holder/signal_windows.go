package holder

import "syscall"

// signal ends the process on a terminate or a kill. Windows has no process group signals.
func (s *server) signal(sig syscall.Signal) {
	if sig != syscall.SIGHUP {
		_ = s.cmd.Process.Kill()
	}
}
