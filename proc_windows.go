//go:build windows

package main

import "os/exec"

// detachSession на Windows — no-op. Парольная аутентификация через SSH_ASKPASS
// здесь не используется (см. buildCmd), а понятия управляющего tty в unix-смысле нет.
func detachSession(cmd *exec.Cmd) {}

// killProcessGroup на Windows просто убивает процесс.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
