package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/howeyc/gopass"
	"github.com/pborman/getopt/v2"
	"github.com/zenthangplus/goccm"
)

const (
	version           = "2.1"
	defaultDevFile    = "./devices.db"
	defaultCmdFile    = "./commands"
	defaultTimeout    = 60
	defaultSSHPort    = 22
	defaultTelnetPort = 23
	defaultJobs       = 1
	connectTimeout    = 15
	retryDelay        = 5 * time.Second
	retryCount        = 1
	pollInterval      = 50 * time.Millisecond
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

	// путь к ssh бинарю — вычисляется один раз при старте
	sshBin = findSSHBinary()
)

// Proto определяет протокол подключения
type Proto int

const (
	ProtoSSH    Proto = iota
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

// Expecter простая реализация expect поверх io.Reader/io.Writer
// с полным контролем над буфером
type Expecter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	w     io.Writer
	debug bool
}

// newExpecter создаёт Expecter и запускает фоновое чтение из r
func newExpecter(r io.Reader, w io.Writer, debug bool) *Expecter {
	e := &Expecter{w: w, debug: debug}
	go func() {
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

// ExpectSwitchCase ждёт совпадения одного из паттернов.
// Возвращает совпавший текст, индекс паттерна и ошибку.
// После матча совпавшая часть удаляется из буфера.
func (e *Expecter) ExpectSwitchCase(patterns []*regexp.Regexp, timeout time.Duration) (string, int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		data := e.buf.String()
		matched := ""
		idx := -1
		for i, re := range patterns {
			loc := re.FindStringIndex(data)
			if loc == nil {
				continue
			}
			matched = data[:loc[1]]
			idx = i
			e.buf.Next(loc[1]) // удаляем совпавшую часть, остаток остаётся
			break
		}
		e.mu.Unlock()

		if idx >= 0 {
			if e.debug {
				fmt.Fprintf(os.Stderr, "Match for RE: %q found\n", patterns[idx].String())
			}
			return matched, idx, nil
		}
		time.Sleep(pollInterval)
	}
	e.mu.Lock()
	data := e.buf.String()
	e.mu.Unlock()
	return data, -1, fmt.Errorf("timeout after %s", timeout)
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

// buildCmd строит exec.Cmd для SSH или Telnet
func buildCmd(cfg Config, device string) *exec.Cmd {
	switch cfg.Proto {
	case ProtoTelnet:
		return exec.Command("telnet", device, strconv.Itoa(cfg.Port))
	default:
		return exec.Command(sshBin,
			"-tt",
			"-o", "StrictHostKeyChecking=no",
			"-o", "CheckHostIP=no",
			"-o", "ConnectTimeout="+strconv.Itoa(connectTimeout),
			"-p", strconv.Itoa(cfg.Port),
			"-l", cfg.Username,
			device,
		)
	}
}

// connectAndRun подключается к устройству и выполняет команды
func connectAndRun(device string, cfg Config) (string, error) {
	cmd := buildCmd(cfg, device)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	go io.Copy(io.Discard, stderr)

	e := newExpecter(stdout, stdin, cfg.Debug)

	// для telnet: login → username → password → prompt
	// для ssh с -tt: сразу prompt или password
	if cfg.Proto == ProtoTelnet {
		_, idx, err := e.ExpectSwitchCase([]*regexp.Regexp{loginRE, promptRE}, cfg.Timeout)
		if err != nil {
			return "", fmt.Errorf("telnet login prompt: %w", err)
		}
		if idx == 0 {
			if err := e.Send(cfg.Username + "\r\n"); err != nil {
				return "", fmt.Errorf("telnet send username: %w", err)
			}
		}
	}

	// ждём пароль или промпт
	_, idx, err := e.ExpectSwitchCase([]*regexp.Regexp{passRE, promptRE}, cfg.Timeout)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	if idx == 0 {
		if err := e.Send(cfg.Password + "\r\n"); err != nil {
			return "", fmt.Errorf("send password: %w", err)
		}
		if _, err := e.Expect(promptRE, cfg.Timeout); err != nil {
			return "", fmt.Errorf("prompt after password: %w", err)
		}
	}

	var buf strings.Builder

	for _, cmd := range cfg.Commands {
		if err := e.Send(cmd + "\r\n"); err != nil {
			return buf.String(), fmt.Errorf("send %q: %w", cmd, err)
		}
		// ждём промпт обрабатывая пагинацию
		for {
			text, idx, err := e.ExpectSwitchCase([]*regexp.Regexp{moreRE, promptRE}, cfg.Timeout)
			if err != nil {
				return buf.String(), fmt.Errorf("expect after %q: %w", cmd, err)
			}
			buf.WriteString(stripANSI(text))
			if idx == 1 {
				break
			}
			// пробел листает страницу, \r\n гарантирует новую строку
			// перед промптом (JunOS иногда печатает промпт без переноса)
			if err := e.Send(" \r\n"); err != nil {
				return buf.String(), fmt.Errorf("send more: %w", err)
			}
		}
	}

	return buf.String(), nil
}

// runDevice выполняет connectAndRun с одним retry и пишет результат в канал
func runDevice(device string, cfg Config, results chan<- Result) {
	var (
		output string
		err    error
	)
	for attempt := 0; attempt <= retryCount; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		output, err = connectAndRun(device, cfg)
		if err == nil {
			break
		}
	}
	if err != nil {
		results <- Result{Device: device, Success: false, Reason: err.Error()}
		return
	}
	results <- Result{Device: device, Success: true, Output: output}
}

func main() {
	optHelp     := getopt.BoolLong("help", '?', "display help")
	optVersion  := getopt.BoolLong("version", 'v', "display version")
	optDevFile  := getopt.StringLong("hosts", 'h', defaultDevFile, "file with devices list")
	optCmdFile  := getopt.StringLong("cmdlist", 'c', "", "file with commands list (mutually exclusive with --cmd)")
	optCmd      := getopt.StringLong("cmd", 'C', "", "inline command (mutually exclusive with --cmdlist)")
	optUsername := getopt.StringLong("username", 'u', "", "username")
	optJobs     := getopt.IntLong("jobs", 'j', defaultJobs, "number of parallel jobs")
	optTimeout  := getopt.IntLong("timeout", 't', defaultTimeout, "timeout in seconds (connect + command)")
	optPort     := getopt.IntLong("port", 'P', 0, "port (default: 22 for SSH, 23 for Telnet)")
	optPassword := getopt.BoolLong("password", 'p', "prompt for password")
	optRun      := getopt.BoolLong("run", 'r', "run commands (required)")
	optDebug    := getopt.BoolLong("debug", 'd', "debug mode")
	optLogFile  := getopt.StringLong("log", 'l', "", "log file (output goes to stdout AND log file)")
	optTelnet   := getopt.BoolLong("telnet", 'T', "use Telnet instead of SSH (default: SSH)")
	getopt.Parse()

	if *optHelp {
		getopt.Usage()
		os.Exit(0)
	}
	if *optVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if !*optRun {
		fmt.Println("\nUse -r flag to actually run commands on devices.\n")
		getopt.Usage()
		os.Exit(0)
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
	if *optLogFile != "" {
		lf, err := os.Create(*optLogFile)
		if err != nil {
			fatal(fmt.Errorf("log file: %w", err))
		}
		defer lf.Close()
		out = io.MultiWriter(os.Stdout, lf)
	}

	// канал результатов
	resultsCh := make(chan Result, len(devices))
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)

	var successes, failures []Result

	// горутина-сборщик — единственный читатель resultsCh
	go func() {
		defer collectorWg.Done()
		for r := range resultsCh {
			fmt.Fprintf(out, "\n##############################################\n")
			fmt.Fprintf(out, "# Device: %s\n", r.Device)
			fmt.Fprintf(out, "##############################################\n")
			if r.Success {
				fmt.Fprint(out, r.Output)
				successes = append(successes, r)
			} else {
				fmt.Fprintf(out, "ERROR: %s\n", r.Reason)
				failures = append(failures, r)
			}
		}
	}()

	// Ctrl+C — обрываем всё сразу
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(out, "\n\nInterrupted.")
		os.Exit(130)
	}()

	// запускаем задачи
	total := len(devices)
	c := goccm.New(*optJobs)
	for i, device := range devices {
		c.Wait()
		d := device
		n := i + 1
		fmt.Fprintf(out, "\n##############################################\n")
		fmt.Fprintf(out, "#    Connecting to %s, [%d/%d]\n", d, n, total)
		fmt.Fprintf(out, "##############################################\n\n")
		go func() {
			defer c.Done()
			runDevice(d, cfg, resultsCh)
		}()
	}
	c.WaitAllDone()
	close(resultsCh)
	collectorWg.Wait()

	// итоговый отчёт
	fmt.Fprintf(out, "\n\n==============================================\n")
	fmt.Fprintf(out, "SUMMARY\n")
	fmt.Fprintf(out, "==============================================\n")
	fmt.Fprintf(out, "Success:      %d\n", len(successes))
	for _, r := range successes {
		fmt.Fprintf(out, "  + %s\n", r.Device)
	}
	fmt.Fprintf(out, "\nUnsuccessful: %d\n", len(failures))
	for _, r := range failures {
		fmt.Fprintf(out, "  - %s  (%s)\n", r.Device, r.Reason)
	}
	fmt.Fprintln(out)
}
