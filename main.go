package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/howeyc/gopass"
	"github.com/pborman/getopt/v2"
	"github.com/zenthangplus/goccm"
)

const (
	version           = "2.7"
	defaultCmdFile    = "./commands"
	defaultTimeout    = 60
	defaultSSHPort    = 22
	defaultTelnetPort = 23
	defaultJobs       = 1
	defaultCharDelay  = 30 // мс между символами в режиме --slow
	retryDelay        = 5 * time.Second
	retryCount        = 1
	pollInterval      = 50 * time.Millisecond
	stderrTailMax     = 4096
)

// параметры пробуждения «тихой» консольной линии (var — чтобы тесты могли ускорить)
var (
	consoleNudgeTries = 3
	consoleNudgeWait  = 3 * time.Second
)

// сентинелы для классификации исхода ожидания (errors.Is)
var (
	errExpectTimeout = errors.New("timeout")
	errExpectClosed  = errors.New("closed")
)

var (
	// универсальный промпт — покрывает Cisco/JunOS/Huawei/MikroTik/Eltex/D-Link.
	// Вторая альтернатива — Huawei VRP system/interface view: строка вида
	// [~host], [*host], [host], [host-GigabitEthernet0/0/1] — заканчивается на ]
	// без >#$. Внутри скобок запрещены пробелы, поэтому JunOS diff-заголовки
	// ([edit interfaces]) и пейджер ([more 51%]) не матчатся; одиночные
	// служебные [edit]/[OK] отсекаются в isRealPrompt (fakeBracketRE)
	promptRE = regexp.MustCompile(`(?m)^([\w<\[][^\n]{0,62}(\][>\s]*[>#$]|[^\s][>#$])|\[[~*]?\w[^\s\[\]]{0,62}\])\s*$`)
	passRE   = regexp.MustCompile(`(?i)assword:`)
	loginRE  = regexp.MustCompile(`(?im)(login|username|user)\s*:\s*$`)
	// строка неуспешного логина — для быстрого отказа в console-режиме
	loginFailRE = regexp.MustCompile(`(?i)(login incorrect|% *login invalid|authentication fail|% *bad password|access denied)`)
	// пагинация — все популярные варианты:
	//   ---(more)---  ---(more 51%)---  --More--  ---- More ----  [more 51%]  <more>
	moreRE = regexp.MustCompile(`(?i)-{2,}\s*\(?\s*more(\s+\d+%)?\s*\)?\s*-{2,}|\[more[^\]]*\]|<\s*more\s*>`)
	// ANSI escape коды (MikroTik и другие)
	ansiRE = regexp.MustCompile(`\x1B\[[\x30-\x3F]*[\x20-\x2F]*[\x40-\x7E]|\x1B[()][AB012]`)
	// строка-фрагмент маршрута, который промпт-RE ловит ложно: 1.2.3.4>
	routeLikeRE = regexp.MustCompile(`\d{1,3}(\.\d{1,3}){3}\s*[>#$]$`)
	// служебные [..]-строки вывода, ложно похожие на Huawei-промпт:
	// [edit] (JunOS show | compare), [OK] (Cisco write mem) и т.п.
	fakeBracketRE = regexp.MustCompile(`(?i)^\[(edit|ok|yes|no|y/n|done)\]$`)
	// схлопывание пробелов для однострочного stderr
	wsRE = regexp.MustCompile(`\s+`)

	// путь к ssh бинарю — вычисляется один раз при старте
	sshBin = findSSHBinary()
)

// Proto определяет протокол подключения
type Proto int

const (
	ProtoSSH Proto = iota
	ProtoTelnet
)

// Device — одно устройство из devices.db (имя для логов + хост/порт для коннекта)
type Device struct {
	Name string // как записано в devices.db (для отчёта/логов)
	Host string // хост для подключения
	Port int    // эффективный порт (из строки host:port либо дефолтный)
}

// Config хранит всё что нужно для подключения и выполнения команд
type Config struct {
	Username string
	Password string
	Proto    Proto
	Debug    bool
	Timeout  time.Duration
	Commands []string
	// CharDelay > 0 включает режим медленной вставки: ввод отправляется
	// посимвольно с этой задержкой (для медленных консоль-серверов, Moxa @9600)
	CharDelay time.Duration
	// console-server режим (Moxa и т.п.): внутрисессионный логин на устройстве
	Console   bool
	LoginUser string
	LoginPass string
	// Live — транслировать сессию в stdout по мере поступления
	Live bool
	// EOL — строка-терминатор команды, разрешённая из --eol флага.
	// Значение уже вычислено (auto применён), всегда одно из "\n", "\r", "\r\n".
	EOL string
}

// Result итог работы по одному устройству
type Result struct {
	Device  string
	Success bool
	Reason  string
	Output  string
}

// jsonResult — представление результата для --json-log
type jsonResult struct {
	Result string `json:"result"`          // success / unsuccess
	Error  string `json:"error,omitempty"` // присутствует только при неуспехе
	Out    string `json:"out"`             // вывод сессии, в любом случае
}

// safeFilename делает имя устройства пригодным для имени файла:
// разделители путей, двоеточия (порт/IPv6) и скобки IPv6 заменяет на _
func safeFilename(device string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", "[", "_", "]", "_")
	return r.Replace(device)
}

// syncWriter — потокобезопасная обёртка над io.Writer (общий мьютекс вывода)
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// tailBuffer хранит только последние max байт записанных данных (для stderr)
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Expecter простая реализация expect поверх io.Reader/io.Writer
// с полным контролем над буфером
type Expecter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	w         io.Writer
	debug     bool
	charDelay time.Duration   // > 0 → посимвольная отправка с задержкой
	ctx       context.Context // для прерывания медленной отправки по Ctrl+C/таймауту
	live      io.Writer       // != nil → транслировать прочитанное в реальном времени
	tail      *tailBuffer     // последние байты сессии — для диагностики ошибок
	eof       chan struct{}   // закрывается когда из reader пришёл EOF
}

// newExpecter создаёт Expecter и запускает фоновое чтение из r
func newExpecter(ctx context.Context, r io.Reader, w io.Writer, debug bool, charDelay time.Duration, live io.Writer) *Expecter {
	e := &Expecter{w: w, debug: debug, charDelay: charDelay, ctx: ctx, live: live, tail: &tailBuffer{max: stderrTailMax}, eof: make(chan struct{})}
	go func() {
		defer close(e.eof)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				e.mu.Lock()
				e.buf.Write(b[:n])
				e.mu.Unlock()
				e.tail.Write(b[:n])
				if debug {
					os.Stderr.Write(b[:n])
				}
				if e.live != nil {
					e.live.Write(b[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return e
}

// tailStr возвращает последние прочитанные из сессии байты (вкл. stderr ssh при
// работе через PTY) — используется для диагностики при ошибке.
func (e *Expecter) tailStr() string {
	if e.tail == nil {
		return ""
	}
	return e.tail.String()
}

// Send отправляет строку в процесс (в debug-режиме логирует содержимое).
func (e *Expecter) Send(s string) error { return e.send(s, false) }

// SendSecret как Send, но в debug-режиме НЕ печатает содержимое (пароли и т.п.):
// вместо плейнтекста выводит заглушку <protected>, чтобы секрет не утёк в
// консоль/лог при запуске с -d.
func (e *Expecter) SendSecret(s string) error { return e.send(s, true) }

// send отправляет строку в процесс. В обычном режиме — одной записью.
// В режиме медленной вставки (charDelay > 0) — побайтово с задержкой между
// символами, чтобы медленный консоль-сервер (Moxa @9600 и т.п.) не терял
// символы. Прерывается по ctx (Ctrl+C/таймаут). При secret=true содержимое
// строки не попадает в debug-вывод.
func (e *Expecter) send(s string, secret bool) error {
	if e.debug {
		if secret {
			fmt.Fprintf(os.Stderr, "Sent: <protected>\n")
		} else {
			fmt.Fprintf(os.Stderr, "Sent: %q\n", s)
		}
	}
	if e.charDelay <= 0 {
		_, err := io.WriteString(e.w, s)
		return err
	}
	b := []byte(s)
	for i := range b {
		if _, err := e.w.Write(b[i : i+1]); err != nil {
			return err
		}
		select {
		case <-e.ctx.Done():
			return e.ctx.Err()
		case <-time.After(e.charDelay):
		}
	}
	return nil
}

// scan один проход по буферу; при совпадении удаляет совпавшую часть
func (e *Expecter) scan(patterns []*regexp.Regexp) (string, int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	data := e.buf.String()
	for i, re := range patterns {
		if loc := re.FindStringIndex(data); loc != nil {
			matched := data[:loc[1]]
			e.buf.Next(loc[1]) // остаток буфера сохраняется
			return matched, i, true
		}
	}
	return data, -1, false
}

// ExpectSwitchCase ждёт совпадения одного из паттернов.
// Возвращает совпавший текст, индекс паттерна и ошибку.
// Если процесс закрыл вывод (EOF) — возвращает ошибку сразу, не ожидая таймаут.
func (e *Expecter) ExpectSwitchCase(patterns []*regexp.Regexp, timeout time.Duration) (string, int, error) {
	deadline := time.Now().Add(timeout)
	for {
		if m, i, ok := e.scan(patterns); ok {
			if e.debug {
				fmt.Fprintf(os.Stderr, "Match for RE: %q found\n", patterns[i].String())
			}
			return m, i, nil
		}

		select {
		case <-e.eof:
			// данных больше не будет — последний (свежий) скан ловит хвост, иначе ошибка
			m, i, ok := e.scan(patterns)
			if ok {
				return m, i, nil
			}
			return m, -1, fmt.Errorf("connection closed before match: %w", errExpectClosed)
		default:
		}

		if time.Now().After(deadline) {
			m, _, _ := e.scan(patterns)
			return m, -1, fmt.Errorf("timeout after %s: %w", timeout, errExpectTimeout)
		}
		time.Sleep(pollInterval)
	}
}

// Expect ждёт совпадения одного паттерна
func (e *Expecter) Expect(re *regexp.Regexp, timeout time.Duration) (string, error) {
	text, _, err := e.ExpectSwitchCase([]*regexp.Regexp{re}, timeout)
	return text, err
}

// printPlan печатает список команд и устройств, на которые они будут разлиты
// (используется и в сухом прогоне без -r, и перед вопросом подтверждения).
func printPlan(commands []string, devices []Device) {
	fmt.Println("\nCommands to run:")
	for _, c := range commands {
		fmt.Printf("  %s\n", c)
	}

	names := make([]string, len(devices))
	for i, d := range devices {
		names[i] = d.Name
	}
	fmt.Printf("\nDevices (%d):\n%s\n\n", len(devices), strings.Join(names, " "))
}

// confirmRun показывает план (printPlan) и спрашивает подтверждение (y/N).
// Любой ответ кроме "y"/"yes" — отказ.
func confirmRun(commands []string, devices []Device) bool {
	printPlan(commands, devices)

	fmt.Print("Continue? (y/N): ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "\nERROR: %s\n\n", err)
	os.Exit(1)
}

// oneLine схлопывает stderr в одну строку, ограничивая длину
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = wsRE.ReplaceAllString(s, " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// isRetriable решает, имеет ли смысл повтор: только транспортные ошибки
// (отказ соединения, нет маршрута и т.п.), но НЕ фаза логина/аутентификации —
// повтор логина бессмысленен и опасен (однопользовательские порты Moxa,
// блокировка учётки).
func isRetriable(err error) bool {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "login") || // login:/console login:/telnet login:/login failed
		strings.Contains(s, "password") || // prompt/send password
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "authentication fail") ||
		strings.Contains(s, "incorrect") {
		return false
	}
	for _, k := range []string{
		"refused", "no route", "timeout", "timed out", "reset",
		"unreachable", "resolve", "not known", "closed", "broken pipe",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// isRealPrompt отсекает ложные матчи промпта: строки-фрагменты маршрутов
// (1.2.3.4>) и служебные [..]-строки вывода ([edit], [OK] и т.п.)
func isRealPrompt(text string) bool {
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return !routeLikeRE.MatchString(last) && !fakeBracketRE.MatchString(last)
}

// readLines читает файл, пропуская пустые строки и комментарии (#)
func readLines(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make([]string, 0, 64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, scanner.Err()
}

// readDevices читает devices.db и парсит необязательный порт в строке (host:port).
// Голый hostname/IPv4 и голый IPv6 (без скобок) трактуются как «порта нет» →
// берётся defPort. Формат с портом: host:port, ipv4:port, [ipv6]:port.
func readDevices(file string, defPort int) ([]Device, error) {
	lines, err := readLines(file)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(lines))
	for _, line := range lines {
		host, port := line, defPort
		if h, p, serr := net.SplitHostPort(line); serr == nil {
			if pn, aerr := strconv.Atoi(p); aerr == nil {
				host, port = h, pn
			}
		}
		out = append(out, Device{Name: line, Host: host, Port: port})
	}
	return out, nil
}

// stripANSI удаляет ANSI escape-коды из строки
func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// findSSHBinary возвращает путь к ssh бинарю
func findSSHBinary() string {
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Windows\System32\OpenSSH\ssh.exe`,
			`C:\Program Files\OpenSSH\ssh.exe`,
			`C:\Program Files (x86)\OpenSSH\ssh.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return "ssh"
}

// buildCmd строит exec.Cmd для SSH или Telnet (с привязкой к context).
// Процесс НЕ запускается здесь — его стартует startSession() на PTY.
func buildCmd(ctx context.Context, cfg Config, dev Device) *exec.Cmd {
	switch cfg.Proto {
	case ProtoTelnet:
		return exec.CommandContext(ctx, "telnet", dev.Host, strconv.Itoa(dev.Port))
	default:
		// Пароль вводится прямо в PTY по приглашению ssh (см. connectAndRun),
		// поэтому работает на всех версиях OpenSSH без сторонних обвязок.
		return exec.CommandContext(ctx, sshBin,
			"-tt",
			"-o", "StrictHostKeyChecking=no",
			"-o", "CheckHostIP=no",
			"-o", "UserKnownHostsFile="+os.DevNull,
			"-o", "ConnectTimeout="+strconv.Itoa(int(cfg.Timeout.Seconds())),
			"-p", strconv.Itoa(dev.Port),
			"-l", cfg.Username,
			dev.Host,
		)
	}
}

// loginDialog проводит внутрисессионный логин: ожидает login:/password:/prompt,
// отвечает заданными кредами и завершается на промпте. При совпадении строки
// неуспешного логина (loginFailRE) сразу возвращает неретраябельную ошибку.
// В console-режиме (nudge=true) будит «тихую» линию одиночным CR с повторами.
//
// Линия консоль-сервера может «залипнуть» от прошлой сессии: оказаться уже
// залогиненной (увидим промпт → сразу готово) или с открытым пейджером
// (увидим more → выходим из него по 'q'). Это делает вход устойчивым к
// прерванному ранее прогону.
func loginDialog(e *Expecter, user, pass, eol string, timeout time.Duration, nudge bool) error {
	pats := []*regexp.Regexp{loginFailRE, loginRE, passRE, promptRE, moreRE}

	// реакция на совпадение; done=true → достигли промпта
	react := func(idx int) (done bool, err error) {
		switch idx {
		case 0: // строка неуспешного логина
			return false, fmt.Errorf("login failed")
		case 1: // login:/username:
			return false, e.Send(user + eol)
		case 2: // password: — секрет, в debug не светим
			return false, e.SendSecret(pass + eol)
		case 3: // промпт — уже вошли (свежий логин либо «залипшая» сессия)
			return true, nil
		case 4: // открытый пейджер от прошлой сессии — выходим из него
			return false, e.Send("q")
		}
		return false, nil
	}

	// фаза пробуждения: для «тихих» линий шлём CR, пока что-нибудь не появится
	if nudge {
		for try := 0; ; try++ {
			if err := e.Send(eol); err != nil {
				return err
			}
			_, idx, err := e.ExpectSwitchCase(pats, consoleNudgeWait)
			if err == nil {
				done, rerr := react(idx)
				if rerr != nil {
					return rerr
				}
				if done {
					return nil
				}
				break // получили login/pass → дальше обычный цикл
			}
			if !errors.Is(err, errExpectTimeout) {
				return err // EOF/обрыв — выходим сразу
			}
			if try+1 >= consoleNudgeTries {
				return fmt.Errorf("no login/prompt after %d nudges: %w", consoleNudgeTries, err)
			}
		}
	}

	for step := 0; step < 6; step++ {
		_, idx, err := e.ExpectSwitchCase(pats, timeout)
		if err != nil {
			return err
		}
		done, rerr := react(idx)
		if rerr != nil {
			return rerr
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("login did not reach prompt")
}

// connectAndRun подключается к устройству и выполняет команды
func connectAndRun(ctx context.Context, dev Device, cfg Config, live io.Writer) (output string, err error) {
	cmd := buildCmd(ctx, cfg, dev)

	// startSession запускает процесс на PTY (unix) и возвращает мастер-сторону:
	// чтение = вывод сессии (включая stderr ssh), запись = ввод. На Windows —
	// обычные пайпы. Процесс стартует внутри startSession.
	session, err := startSession(cmd)
	if err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	procDone := make(chan struct{})
	go func() { cmd.Wait(); close(procDone) }()

	e := newExpecter(ctx, session, session, cfg.Debug, cfg.CharDelay, live)

	// выполнится ПЕРВЫМ: закрываем сессию, убиваем группу процессов, ждём Wait
	defer func() {
		session.Close()
		killProcessGroup(cmd)
		<-procDone
	}()
	// выполнится ПОСЛЕДНИМ (после kill): добавляем в ошибку хвост сессии —
	// при работе через PTY туда попадает и stderr ssh ("Permission denied" и т.п.)
	defer func() {
		if err != nil {
			if t := oneLine(e.tailStr()); t != "" {
				err = fmt.Errorf("%w [%s]", err, t)
			}
		}
	}()

	// Конец строки берётся из cfg.EOL, вычисленного в run() из --eol / auto-дефолта.
	// auto: обычный режим → "\n", console (-M) → "\r".
	// Прямое указание --eol lf|cr|crlf переопределяет дефолт.
	// Предупреждение: crlf на PTY даёт ДВА Enter (CR→NL + NL) и может
	// вызвать рассинхрон сессии — используйте только если железка требует CRLF.
	eol := cfg.EOL

	switch {
	case cfg.Console:
		// console-server. Для SSH сначала отвечаем на парольный запрос самого ssh
		// (аккаунт консоль-сервера/Moxa), который теперь приходит в PTY. Для telnet
		// к raw-порту транспортной аутентификации нет — сразу логин устройства.
		if cfg.Proto == ProtoSSH {
			_, idx, terr := e.ExpectSwitchCase([]*regexp.Regexp{passRE, loginFailRE}, cfg.Timeout)
			switch {
			case terr == nil && idx == 1:
				return "", fmt.Errorf("console login: transport auth failed")
			case terr == nil:
				if err = e.SendSecret(cfg.Password + eol); err != nil {
					return "", fmt.Errorf("console login: send transport password: %w", err)
				}
			case !errors.Is(terr, errExpectTimeout):
				return "", fmt.Errorf("console login: %w", terr)
				// таймаут парольного запроса → вероятно вход по ключу, идём к device-логину
			}
		}
		// device-логин отдельными кредами устройства, с пробуждением «тихой» линии
		if err = loginDialog(e, cfg.LoginUser, cfg.LoginPass, eol, cfg.Timeout, true); err != nil {
			return "", fmt.Errorf("console login: %w", err)
		}
	case cfg.Proto == ProtoTelnet:
		// telnet: внутрисессионный логин кредами -u/-p (без nudge)
		if err = loginDialog(e, cfg.Username, cfg.Password, eol, cfg.Timeout, false); err != nil {
			return "", fmt.Errorf("telnet login: %w", err)
		}
	default:
		// ssh: пароль спрашивается в PTY; некоторые устройства спрашивают повторно в сессии
		_, idx, lerr := e.ExpectSwitchCase([]*regexp.Regexp{passRE, promptRE}, cfg.Timeout)
		if lerr != nil {
			return "", fmt.Errorf("login: %w", lerr)
		}
		if idx == 0 {
			if err = e.SendSecret(cfg.Password + eol); err != nil {
				return "", fmt.Errorf("send password: %w", err)
			}
			if _, perr := e.Expect(promptRE, cfg.Timeout); perr != nil {
				return "", fmt.Errorf("prompt after password: %w", perr)
			}
		}
	}

	var buf strings.Builder

	for i, cmdStr := range cfg.Commands {
		last := i == len(cfg.Commands)-1
		if err = e.Send(cmdStr + eol); err != nil {
			return buf.String(), fmt.Errorf("send %q: %w", cmdStr, err)
		}
		// ждём промпт обрабатывая пагинацию
		for {
			text, mi, eerr := e.ExpectSwitchCase([]*regexp.Regexp{moreRE, promptRE}, cfg.Timeout)
			if eerr != nil {
				// последняя команда закрыла сессию (logout/exit/reload) — это норма,
				// а не ошибка: добираем хвост вывода и выходим успешно
				if last && errors.Is(eerr, errExpectClosed) {
					buf.WriteString(stripANSI(text))
					return buf.String(), nil
				}
				return buf.String(), fmt.Errorf("expect after %q: %w", cmdStr, eerr)
			}
			buf.WriteString(stripANSI(text))
			if mi == 1 {
				// проверяем, что это настоящий промпт, а не строка маршрута 1.2.3.4>
				if isRealPrompt(text) {
					break
				}
				continue // ложное срабатывание — читаем дальше
			}
			// пробел листает страницу, eol гарантирует новую строку
			// перед промптом (JunOS иногда печатает промпт без переноса)
			if err = e.Send(" " + eol); err != nil {
				return buf.String(), fmt.Errorf("send more: %w", err)
			}
		}
	}

	return buf.String(), nil
}

// runDevice выполняет connectAndRun с retry (только на транспортных ошибках)
func runDevice(ctx context.Context, dev Device, cfg Config, live io.Writer, results chan<- Result) {
	var (
		output string
		err    error
	)
	for attempt := 0; ; attempt++ {
		output, err = connectAndRun(ctx, dev, cfg, live)
		if err == nil {
			results <- Result{Device: dev.Name, Success: true, Output: output}
			return
		}
		if attempt >= retryCount || ctx.Err() != nil || !isRetriable(err) {
			break
		}
		select {
		case <-time.After(retryDelay):
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	results <- Result{Device: dev.Name, Success: false, Reason: err.Error(), Output: output}
}

func main() {
	os.Exit(run())
}

func run() int {
	optHelp := getopt.BoolLong("help", '?', "display help")
	optVersion := getopt.BoolLong("version", 'v', "display version")
	optDevFile := getopt.StringLong("hosts", 'h', "", "file with devices list (required)")
	optCmdFile := getopt.StringLong("cmdlist", 'c', "", "file with commands list (mutually exclusive with --cmd)")
	optCmd := getopt.StringLong("cmd", 'C', "", "inline command (mutually exclusive with --cmdlist)")
	optUsername := getopt.StringLong("username", 'u', "", "username")
	optJobs := getopt.IntLong("jobs", 'j', defaultJobs, "number of parallel jobs")
	optTimeout := getopt.IntLong("timeout", 't', defaultTimeout, "timeout in seconds (connect + command)")
	optPort := getopt.IntLong("port", 'P', 0, "default port for lines without one (default: 22 SSH / 23 Telnet)")
	optPassword := getopt.BoolLong("password", 'p', "prompt for password")
	optRun := getopt.BoolLong("run", 'r', "actually run commands on devices (without it: dry run, only show the plan)")
	optDebug := getopt.BoolLong("debug", 'd', "debug mode")
	optLogFile := getopt.StringLong("log", 'l', "", "log file (output goes to stdout AND log file)")
	optJSONLog := getopt.StringLong("json-log", 'J', "", "write results to a JSON file (keyed by device)")
	optLogDir := getopt.StringLong("log-dir", 'D', "", "directory: one log file per device (<name>.log)")
	optTelnet := getopt.BoolLong("telnet", 'T', "use Telnet instead of SSH (default: SSH)")
	optSlow := getopt.BoolLong("slow", 'S', "slow paste: send input char-by-char (for slow console servers, e.g. Moxa @9600)")
	optCharDelay := getopt.IntLong("char-delay", 0, defaultCharDelay, "inter-character delay in ms for --slow")
	optConsole := getopt.BoolLong("console", 'M', "console-server mode (Moxa etc.): in-session device login after connect; forces -j 1")
	optLoginUser := getopt.StringLong("login-user", 0, "", "device login username for --console (default: --username)")
	optLoginPass := getopt.BoolLong("login-pass", 0, "prompt for a separate device login password for --console")
	optLive := getopt.BoolLong("live", 0, "stream session output live as it happens (forces -j 1; implied by --console)")
	optEOL := getopt.StringLong("eol", 0, "auto", "line ending: auto (default), lf, cr, crlf")
	optExtreme := getopt.BoolLong("extreme", 'e', "skip the confirmation prompt and run immediately")
	getopt.Parse()

	if *optHelp {
		getopt.Usage()
		return 0
	}
	if *optVersion {
		fmt.Println(version)
		return 0
	}
	if *optCmd != "" && *optCmdFile != "" {
		fatal(fmt.Errorf("--cmd and --cmdlist are mutually exclusive"))
	}
	if *optDevFile == "" {
		fatal(fmt.Errorf("--hosts is required: specify a file with devices list"))
	}

	// протокол и порт по умолчанию (для строк devices.db без явного порта)
	proto := ProtoSSH
	port := defaultSSHPort
	if *optTelnet {
		proto = ProtoTelnet
		port = defaultTelnetPort
	}
	if *optPort != 0 {
		port = *optPort
	}

	// console подразумевает live; live и console требуют -j 1
	console := *optConsole
	live := *optLive || console

	// username
	username := *optUsername
	if username == "" {
		u, err := user.Current()
		if err != nil {
			fatal(err)
		}
		username = u.Username
	}

	// читаем устройства (с разбором host:port)
	devices, err := readDevices(*optDevFile, port)
	if err != nil {
		fatal(fmt.Errorf("devices file: %w", err))
	}
	if len(devices) == 0 {
		fatal(fmt.Errorf("devices file %s is empty", *optDevFile))
	}

	// читаем команды
	var commands []string
	switch {
	case *optCmd != "":
		commands = []string{*optCmd}
	case *optCmdFile != "":
		commands, err = readLines(*optCmdFile)
		if err != nil {
			fatal(fmt.Errorf("commands file: %w", err))
		}
	default:
		commands, err = readLines(defaultCmdFile)
		if err != nil {
			fatal(fmt.Errorf("commands file: %w", err))
		}
	}
	if len(commands) == 0 {
		fatal(fmt.Errorf("commands list is empty"))
	}

	// без -r/--run — только показываем план (сухой прогон), ничего не льём
	if !*optRun {
		printPlan(commands, devices)
		fmt.Println("Dry run: use -r/--run to execute these commands on the devices above.")
		return 0
	}

	// защита от случайного разлива на прод: показываем план и просим
	// подтверждение, если не указан --extreme
	if !*optExtreme && !confirmRun(commands, devices) {
		fmt.Println("Aborted.")
		return 1
	}

	// пароль (транспорт: ssh-вход или telnet)
	var password string
	if *optPassword {
		fmt.Printf("Enter password: ")
		p, err := gopass.GetPasswd()
		if err != nil {
			fatal(err)
		}
		password = string(p)
	}

	// креды устройства для console-логина
	loginUser := *optLoginUser
	if loginUser == "" {
		loginUser = username
	}
	loginPass := password
	if console && *optLoginPass {
		fmt.Printf("Enter device login password: ")
		p, err := gopass.GetPasswd()
		if err != nil {
			fatal(err)
		}
		loginPass = string(p)
	}

	// режим медленной вставки
	var charDelay time.Duration
	if *optSlow {
		charDelay = time.Duration(*optCharDelay) * time.Millisecond
	}

	// конец строки: резолвим auto-дефолт и проверяем допустимые значения
	eolStr := strings.ToLower(strings.TrimSpace(*optEOL))
	var eol string
	switch eolStr {
	case "auto":
		// auto: console-режим использует CR (совместимость с serial/Moxa),
		// всё остальное — LF (один Enter на PTY, фикс рассинхрона v2.4)
		if console {
			eol = "\r"
		} else {
			eol = "\n"
		}
	case "lf":
		eol = "\n"
	case "cr":
		eol = "\r"
	case "crlf":
		// ПРЕДУПРЕЖДЕНИЕ: на PTY \r\n = два Enter → двойной промпт → рассинхрон.
		// Оставлено как escape-hatch для нестандартных транспортов.
		fmt.Fprintln(os.Stderr, "WARNING: --eol crlf produces two Enter keystrokes on a PTY and may cause session desync on most devices")
		eol = "\r\n"
	default:
		fatal(fmt.Errorf("--eol: unknown value %q, must be one of: auto, lf, cr, crlf", *optEOL))
	}
	if *optDebug {
		eolNames := map[string]string{"\n": "lf", "\r": "cr", "\r\n": "crlf"}
		fmt.Fprintf(os.Stderr, "EOL: %s\n", eolNames[eol])
	}

	// debug / console / live форсят один поток
	jobs := *optJobs
	if jobs != 1 && (*optDebug || console || live) {
		reason := "debug"
		if console || live {
			reason = "console/live"
		}
		fmt.Fprintf(os.Stderr, "%s mode: forcing -j 1\n", reason)
		jobs = 1
	}

	cfg := Config{
		Username:  username,
		Password:  password,
		Proto:     proto,
		Debug:     *optDebug,
		Timeout:   time.Duration(*optTimeout) * time.Second,
		Commands:  commands,
		CharDelay: charDelay,
		Console:   console,
		LoginUser: loginUser,
		LoginPass: loginPass,
		Live:      live,
		EOL:       eol,
	}

	// вывод: stdout + опциональный файл
	out := io.Writer(os.Stdout)
	var lf *os.File
	if *optLogFile != "" {
		// 0o600: лог содержит полный вывод сессии (вкл. show running-config с
		// секретами) — закрываем чтение для остальных пользователей хоста
		f, err := os.OpenFile(*optLogFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fatal(fmt.Errorf("log file: %w", err))
		}
		lf = f
		defer lf.Close()
		out = io.MultiWriter(os.Stdout, lf)
	}

	// единый сериализованный писатель — блоки не перемешиваются
	var outMu sync.Mutex
	emit := func(s string) { // stdout (+ -l): баннеры, статус, summary
		outMu.Lock()
		io.WriteString(out, s)
		outMu.Unlock()
	}
	emitLog := func(s string) { // только в -l (чистый блок в live-режиме)
		if lf == nil {
			return
		}
		outMu.Lock()
		io.WriteString(lf, s)
		outMu.Unlock()
	}
	// live: сырой поток сессии — только в stdout, в реальном времени
	var liveOut io.Writer
	if live {
		liveOut = &syncWriter{mu: &outMu, w: os.Stdout}
	}

	// каталог для персональных логов (по файлу на устройство)
	// 0o700: содержимое — вывод сессий с возможными секретами
	if *optLogDir != "" {
		if err := os.MkdirAll(*optLogDir, 0o700); err != nil {
			fatal(fmt.Errorf("log dir: %w", err))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// канал результатов
	resultsCh := make(chan Result, len(devices))
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)

	var successes, failures []Result

	// горутина-сборщик — единственный читатель resultsCh
	go func() {
		defer collectorWg.Done()
		for r := range resultsCh {
			var b strings.Builder
			fmt.Fprintf(&b, "\n##############################################\n")
			fmt.Fprintf(&b, "# Device: %s\n", r.Device)
			fmt.Fprintf(&b, "##############################################\n")
			if r.Success {
				b.WriteString(r.Output)
				successes = append(successes, r)
			} else {
				fmt.Fprintf(&b, "ERROR: %s\n", r.Reason)
				failures = append(failures, r)
			}
			block := b.String()

			if cfg.Live {
				// тело уже шло вживую в stdout; в лог пишем чистый блок,
				// ошибку дублируем и в stdout (её в живом потоке может быть не видно)
				if r.Success {
					emitLog(block)
				} else {
					emit(block)
				}
			} else {
				emit(block)
			}

			// персональный лог-файл устройства (--log-dir)
			// 0o600: тот же чувствительный вывод сессии
			if *optLogDir != "" {
				path := filepath.Join(*optLogDir, safeFilename(r.Device)+".log")
				if werr := os.WriteFile(path, []byte(block), 0o600); werr != nil {
					fmt.Fprintf(os.Stderr, "WARN: write %s: %s\n", path, werr)
				}
			}
		}
	}()

	// Ctrl+C — отменяем контекст (дочерние процессы убиваются),
	// не делаем os.Exit, чтобы отработали defer'ы (флаш лога, закрытие файла)
	var interrupted int32
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		atomic.StoreInt32(&interrupted, 1)
		emit("\n\nInterrupted, aborting active sessions...\n")
		cancel()
	}()

	// запускаем задачи
	total := len(devices)
	c := goccm.New(jobs)
	for i, device := range devices {
		if ctx.Err() != nil {
			break
		}
		c.Wait()
		d := device
		n := i + 1
		emit(fmt.Sprintf("\n##############################################\n#    Connecting to %s, [%d/%d]\n##############################################\n\n", d.Name, n, total))
		go func() {
			defer c.Done()
			runDevice(ctx, d, cfg, liveOut, resultsCh)
		}()
	}
	c.WaitAllDone()
	close(resultsCh)
	collectorWg.Wait()

	// итоговый отчёт
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n==============================================\n")
	fmt.Fprintf(&sb, "SUMMARY\n")
	fmt.Fprintf(&sb, "==============================================\n")
	fmt.Fprintf(&sb, "Success:      %d\n", len(successes))
	for _, r := range successes {
		fmt.Fprintf(&sb, "  + %s\n", r.Device)
	}
	fmt.Fprintf(&sb, "\nUnsuccessful: %d\n", len(failures))
	for _, r := range failures {
		fmt.Fprintf(&sb, "  - %s  (%s)\n", r.Device, r.Reason)
	}
	sb.WriteString("\n")
	emit(sb.String())

	if lf != nil {
		lf.Sync()
	}

	// JSON-лог (--json-log): единый документ, пишется после сбора всех результатов
	// 0o600: содержит вывод сессий (поле out) с возможными секретами
	if *optJSONLog != "" {
		m := make(map[string]jsonResult, len(successes)+len(failures))
		for _, r := range successes {
			m[r.Device] = jsonResult{Result: "success", Out: r.Output}
		}
		for _, r := range failures {
			m[r.Device] = jsonResult{Result: "unsuccess", Error: r.Reason, Out: r.Output}
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: json marshal: %s\n", err)
		} else if err := os.WriteFile(*optJSONLog, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: write json log %s: %s\n", *optJSONLog, err)
		}
	}

	if atomic.LoadInt32(&interrupted) == 1 {
		return 130
	}
	return 0
}
