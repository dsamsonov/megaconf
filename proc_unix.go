//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachSession запускает процесс в новой сессии (setsid) без управляющего
// терминала. Это нужно, чтобы OpenSSH не мог прочитать пароль из /dev/tty и
// гарантированно использовал SSH_ASKPASS — в том числе на старых клиентах
// (< 8.4), где нет SSH_ASKPASS_REQUIRE. Новая сессия = новая группа процессов
// (pgid == pid), что также позволяет аккуратно убить всю группу.
func detachSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// killProcessGroup убивает всю группу процессов (ssh/telnet и любых их потомков,
// например ProxyJump или askpass-хелпер). Поскольку процесс стартовал с Setsid,
// его pgid == pid, поэтому отрицательный pid адресует группу.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// на всякий случай — если группового убийства не вышло, бьём по pid
		_ = cmd.Process.Kill()
	}
}
