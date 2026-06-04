//go:build !windows

package main

import (
	"io"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// startSession запускает команду, подключённую к псевдотерминалу (PTY), и
// возвращает мастер-сторону как единый io.ReadWriteCloser (чтение = вывод
// процесса, запись = ввод процессу; stderr процесса тоже попадает сюда).
//
// PTY критичен: с настоящим управляющим терминалом `ssh -tt` ведёт себя как
// при живом входе — сам спрашивает пароль в терминале (askpass/SSH_ASKPASS не
// нужны ни на одной версии OpenSSH), а удалённая сессия получает интерактивный
// tty, поэтому консоль-серверы (Moxa и пр.) отдают баннер/приглашение. pty.Start
// также стартует процесс в новой сессии (setsid) с pty в роли ctty — это даёт
// и собственную группу процессов для аккуратного группового kill.
func startSession(cmd *exec.Cmd) (io.ReadWriteCloser, error) {
	return pty.Start(cmd)
}

// killProcessGroup убивает всю группу процессов (ssh/telnet и любых их потомков,
// например ProxyJump). Процесс стартовал в своей сессии (pty.Start → Setsid),
// поэтому pgid == pid и отрицательный pid адресует группу.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
