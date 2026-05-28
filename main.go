package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	version           = "2.2"
	defaultDevFile    = "./devices.db"
	defaultCmdFile    = "./commands"
	defaultTimeout    = 60
	defaultSSHPort    = 22
	defaultTelnetPort = 23
	defaultJobs       = 1
	retryDelay        = 5 * time.Second
	retryCount        = 1
	pollInterval      = 50 * time.Millisecond
	stderrTailMax     = 4096
)

var (
	// универсальный промпт — покрывает Cisco/JunOS/Huawei/MikroTik/Eltex/D-Link
	// исключает JunOS diff строки и маршруты вида "via 1.2.3.4 >"
	promptRE = regexp.MustCompile(`(?m)^[\w<\[][^\n]{0,62}(\][>\s]*[>#$]|[^\s][>#$])\s*$`)
	passRE   = regexp.MustCompile(`(?i)assword:`)
	loginRE  = regexp.MustCompile(`(?im)(login|username|user)\s*:\s*$`)
	// пагинация — все популярные варианты включая JunOS ---(more)---
	moreRE = regexp.MustCompile(`(?i)-+\s*\(?\s*more\s*\)?\s*-+|\[more [0-9]+%\]|<more>`)
	// ANSI escape коды (MikroTik и другие)
	ansiRE = regexp.MustCompile(`\x1B\[[\x30-\x3F]*[\x20-\x2F]*[\x40-\x7E]|\x1B[()][AB012]`)
	// строка-фрагмент маршрута, который промпт-RE ловит ложно: 1.2.3.4>
	routeLikeRE = regexp.MustCompile(`\d{1,3}(\.\d{1,3}){3}\s*[>#$]$`)
	// схлопывание пробелов для однострочного stderr
	wsRE = regexp.MustCompile(`\s+`)

	// путь к ssh бинарю — вычисляется один раз при старте
	sshBin = findSSHBinary()
	// путь к собственному бинарю — используется как SSH_ASKPASS-хелпер
	selfBin = findSelf()
)

// Proto определяет протокол подключения
type Proto int

const (
	ProtoSSH Proto = iota
	ProtoTelnet
)

// Config хранит всё что нужно для подключения и выполнения команд
type Config struct {
	Username string
	Password string
	Port     int
	Proto    Proto
	Debug    bool
	Timeout  time.Duration
	Commands []string
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
// точки (IP) сохраняет, разделители путей и двоеточия (IPv6) заменяет на _
func safeFilename(device string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_")
	return r.Replace(device)
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
	mu    sync.Mutex
	buf   bytes.Buffer
	w     io.Writer
	debug bool
	eof   chan struct{} // закрывается когда из reader пришёл EOF
}

// newExpecter создаёт Expecter и запускает фоновое чтение из r
func newExpecter(r io.Reader, w io.Writer, debug bool) *Expecter {
	e := &Expecter{w: w, debug: debug, eof: make(chan struct{})}
	go func() {
		defer close(e.eof)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				e.mu.Lock()
				e.buf.Write(b[:n])
				e.mu.Unlock()
				if debug {
					os.Stderr.Write(b[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return e
}

// Send отправляет строку в процесс
func (e *Expecter) Send(s string) error {
	if e.debug {
		fmt.Fprintf(os.Stderr, "Sent: %q\n", s)
	}
	_, err := fmt.Fprint(e.w, s)
	return err
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
			return m, -1, fmt.Errorf("connection closed before match")
		default:
		}

		if time.Now().After(deadline) {
			m, _, _ := e.scan(patterns)
			return m, -1, fmt.Errorf("timeout after %s", timeout)
		}
		time.Sleep(pollInterval)
	}
}

// Expect ждёт совпадения одного паттерна
func (e *Expecter) Expect(re *regexp.Regexp, timeout time.Duration) (string, error) {
	text, _, err := e.ExpectSwitchCase([]*regexp.Regexp{re}, timeout)
	return text, err
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

// isRetriable решает, имеет ли смысл повтор: только транспортные ошибки,
// не ошибки аутентификации
func isRetriable(err error) bool {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "permission denied") ||
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

// isRealPrompt отсекает ложный матч промпта на строке-фрагменте маршрута (1.2.3.4>)
func isRealPrompt(text string) bool {
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	return !routeLikeRE.MatchString(last)
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

// findSelf возвращает путь к собственному бинарю (для роли SSH_ASKPASS)
func findSelf() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// buildCmd строит exec.Cmd для SSH или Telnet (с привязкой к context)
func buildCmd(ctx context.Context, cfg Config, device string) *exec.Cmd {
	switch cfg.Proto {
	case ProtoTelnet:
		return exec.CommandContext(ctx, "telnet", device, strconv.Itoa(cfg.Port))
	default:
		cmd := exec.CommandContext(ctx, sshBin,
			"-tt",
			"-o", "StrictHostKeyChecking=no",
			"-o", "CheckHostIP=no",
			"-o", "UserKnownHostsFile="+os.DevNull,
			"-o", "ConnectTimeout="+strconv.Itoa(int(cfg.Timeout.Seconds())),
			"-p", strconv.Itoa(cfg.Port),
			"-l", cfg.Username,
			device,
		)
		// настоящий SSH-пароль на уровне auth через SSH_ASKPASS:
		// бинарь служит сам себе askpass-хелпером (см. shim в main).
		// Требует OpenSSH >= 8.4; на Windows механизм отличается — пропускаем.
		if runtime.GOOS != "windows" && cfg.Password != "" && selfBin != "" {
			cmd.Env = append(os.Environ(),
				"SSH_ASKPASS="+selfBin,
				"SSH_ASKPASS_REQUIRE=force",
				"MEGACONF_ASKPASS=1",
				"MEGACONF_ASKPASS_PASS="+cfg.Password,
			)
		}
		return cmd
	}
}

// connectAndRun подключается к устройству и выполняет команды
func connectAndRun(ctx context.Context, device string, cfg Config) (output string, err error) {
	cmd := buildCmd(ctx, cfg, device)

	// stderr собираем в кольцевой буфер (а не выбрасываем),
	// чтобы причина ошибки попала в отчёт
	tb := &tailBuffer{max: stderrTailMax}
	cmd.Stderr = tb

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}

	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	procDone := make(chan struct{})
	go func() { cmd.Wait(); close(procDone) }()

	// регистрируется первым → выполнится ПОСЛЕДНИМ:
	// к этому моменту процесс убит и cmd.Stderr полностью прочитан
	defer func() {
		if err != nil {
			if tail := oneLine(tb.String()); tail != "" {
				err = fmt.Errorf("%w [%s]", err, tail)
			}
		}
	}()
	// выполнится ПЕРВЫМ: закрываем stdin, убиваем процесс, ждём Wait
	defer func() {
		stdin.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-procDone
	}()

	e := newExpecter(stdout, stdin, cfg.Debug)

	// для telnet: login → username → password → prompt
	// для ssh с -tt: сразу prompt или password (если устройство спрашивает в сессии)
	if cfg.Proto == ProtoTelnet {
		_, idx, lerr := e.ExpectSwitchCase([]*regexp.Regexp{loginRE, promptRE}, cfg.Timeout)
		if lerr != nil {
			return "", fmt.Errorf("telnet login prompt: %w", lerr)
		}
		if idx == 0 {
			if err = e.Send(cfg.Username + "\r\n"); err != nil {
				return "", fmt.Errorf("telnet send username: %w", err)
			}
		}
	}

	// ждём пароль или промпт
	_, idx, lerr := e.ExpectSwitchCase([]*regexp.Regexp{passRE, promptRE}, cfg.Timeout)
	if lerr != nil {
		return "", fmt.Errorf("login: %w", lerr)
	}
	if idx == 0 {
		if err = e.Send(cfg.Password + "\r\n"); err != nil {
			return "", fmt.Errorf("send password: %w", err)
		}
		if _, perr := e.Expect(promptRE, cfg.Timeout); perr != nil {
			return "", fmt.Errorf("prompt after password: %w", perr)
		}
	}

	var buf strings.Builder

	for _, cmdStr := range cfg.Commands {
		if err = e.Send(cmdStr + "\r\n"); err != nil {
			return buf.String(), fmt.Errorf("send %q: %w", cmdStr, err)
		}
		// ждём промпт обрабатывая пагинацию
		for {
			text, mi, eerr := e.ExpectSwitchCase([]*regexp.Regexp{moreRE, promptRE}, cfg.Timeout)
			if eerr != nil {
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
			// пробел листает страницу, \r\n гарантирует новую строку
			// перед промптом (JunOS иногда печатает промпт без переноса)
			if err = e.Send(" \r\n"); err != nil {
				return buf.String(), fmt.Errorf("send more: %w", err)
			}
		}
	}

	return buf.String(), nil
}

// runDevice выполняет connectAndRun с retry (только на транспортных ошибках)
func runDevice(ctx context.Context, device string, cfg Config, results chan<- Result) {
	var (
		output string
		err    error
	)
	for attempt := 0; ; attempt++ {
		output, err = connectAndRun(ctx, device, cfg)
		if err == nil {
			results <- Result{Device: device, Success: true, Output: output}
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
	results <- Result{Device: device, Success: false, Reason: err.Error(), Output: output}
}

func main() {
	// SSH_ASKPASS-shim: при вызове через ssh печатаем пароль из env и выходим
	if os.Getenv("MEGACONF_ASKPASS") == "1" {
		fmt.Println(os.Getenv("MEGACONF_ASKPASS_PASS"))
		return
	}
	os.Exit(run())
}

func run() int {
	optHelp := getopt.BoolLong("help", '?', "display help")
	optVersion := getopt.BoolLong("version", 'v', "display version")
	optDevFile := getopt.StringLong("hosts", 'h', defaultDevFile, "file with devices list")
	optCmdFile := getopt.StringLong("cmdlist", 'c', "", "file with commands list (mutually exclusive with --cmd)")
	optCmd := getopt.StringLong("cmd", 'C', "", "inline command (mutually exclusive with --cmdlist)")
	optUsername := getopt.StringLong("username", 'u', "", "username")
	optJobs := getopt.IntLong("jobs", 'j', defaultJobs, "number of parallel jobs")
	optTimeout := getopt.IntLong("timeout", 't', defaultTimeout, "timeout in seconds (connect + command)")
	optPort := getopt.IntLong("port", 'P', 0, "port (default: 22 for SSH, 23 for Telnet)")
	optPassword := getopt.BoolLong("password", 'p', "prompt for password")
	optRun := getopt.BoolLong("run", 'r', "run commands (required)")
	optDebug := getopt.BoolLong("debug", 'd', "debug mode")
	optLogFile := getopt.StringLong("log", 'l', "", "log file (output goes to stdout AND log file)")
	optJSONLog := getopt.StringLong("json-log", 'J', "", "write results to a JSON file (keyed by device)")
	optLogDir := getopt.StringLong("log-dir", 'D', "", "directory: one log file per device (<name>.log)")
	optTelnet := getopt.BoolLong("telnet", 'T', "use Telnet instead of SSH (default: SSH)")
	getopt.Parse()

	if *optHelp {
		getopt.Usage()
		return 0
	}
	if *optVersion {
		fmt.Println(version)
		return 0
	}
	if !*optRun {
		fmt.Print("\nUse -r flag to actually run commands on devices.\n\n")
		getopt.Usage()
		return 0
	}
	if *optCmd != "" && *optCmdFile != "" {
		fatal(fmt.Errorf("--cmd and --cmdlist are mutually exclusive"))
	}

	// протокол и порт
	proto := ProtoSSH
	port := defaultSSHPort
	if *optTelnet {
		proto = ProtoTelnet
		port = defaultTelnetPort
	}
	if *optPort != 0 {
		port = *optPort
	}

	// username
	username := *optUsername
	if username == "" {
		u, err := user.Current()
		if err != nil {
			fatal(err)
		}
		username = u.Username
	}

	// читаем устройства
	devices, err := readLines(*optDevFile)
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

	// пароль
	var password string
	if *optPassword {
		fmt.Printf("Enter password: ")
		p, err := gopass.GetPasswd()
		if err != nil {
			fatal(err)
		}
		password = string(p)
	}

	// в debug-режиме форсим один поток — иначе сырой вывод горутин мешается
	jobs := *optJobs
	if *optDebug && jobs != 1 {
		fmt.Fprintln(os.Stderr, "debug mode: forcing -j 1 for readable output")
		jobs = 1
	}

	cfg := Config{
		Username: username,
		Password: password,
		Port:     port,
		Proto:    proto,
		Debug:    *optDebug,
		Timeout:  time.Duration(*optTimeout) * time.Second,
		Commands: commands,
	}

	// вывод: stdout + опциональный файл
	out := io.Writer(os.Stdout)
	var lf *os.File
	if *optLogFile != "" {
		f, err := os.Create(*optLogFile)
		if err != nil {
			fatal(fmt.Errorf("log file: %w", err))
		}
		lf = f
		defer lf.Close()
		out = io.MultiWriter(os.Stdout, lf)
	}

	// единый сериализованный писатель — блоки не перемешиваются
	var outMu sync.Mutex
	emit := func(s string) {
		outMu.Lock()
		io.WriteString(out, s)
		outMu.Unlock()
	}

	// каталог для персональных логов (по файлу на устройство)
	if *optLogDir != "" {
		if err := os.MkdirAll(*optLogDir, 0o755); err != nil {
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
			emit(block)

			// персональный лог-файл устройства (--log-dir)
			if *optLogDir != "" {
				path := filepath.Join(*optLogDir, safeFilename(r.Device)+".log")
				if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "WARN: write %s: %s\n", path, err)
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
		emit(fmt.Sprintf("\n##############################################\n#    Connecting to %s, [%d/%d]\n##############################################\n\n", d, n, total))
		go func() {
			defer c.Done()
			runDevice(ctx, d, cfg, resultsCh)
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
		} else if err := os.WriteFile(*optJSONLog, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: write json log %s: %s\n", *optJSONLog, err)
		}
	}

	if atomic.LoadInt32(&interrupted) == 1 {
		return 130
	}
	return 0
}
