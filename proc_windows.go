//go:build windows

package main

import (
	"io"
	"os/exec"
)

// winSession объединяет stdin/stdout процесса в один io.ReadWriteCloser.
// На Windows PTY-механизм (ConPTY) отличается и парольная SSH-аутентификация
// здесь не поддерживается — используйте ключи/agent. Это совместимый фолбэк
// на обычных пайпах.
type winSession struct {
	in  io.WriteCloser
	out io.ReadCloser
}

func (w *winSession) Read(p []byte) (int, error)  { return w.out.Read(p) }
func (w *winSession) Write(p []byte) (int, error) { return w.in.Write(p) }
func (w *winSession) Close() error {
	_ = w.in.Close()
	return w.out.Close()
}

// startSession запускает процесс с пайпами на stdin/stdout и возвращает их
// как единый ReadWriteCloser. Процесс стартуется здесь (как и pty.Start на unix).
func startSession(cmd *exec.Cmd) (io.ReadWriteCloser, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &winSession{in: in, out: out}, nil
}

// killProcessGroup на Windows просто убивает процесс.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
