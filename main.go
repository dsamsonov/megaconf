package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/goexpect"
	"github.com/howeyc/gopass"
	"github.com/pborman/getopt/v2"
	"github.com/zenthangplus/goccm"
	"google.golang.org/grpc/codes"
)

const (
	version           = "2.1"
	defaultDevFile    = "./devices.db"
	defaultCmdFile    = "./commands"
	defaultTimeout    = 60
	defaultSSHPort    = 22
	defaultTelnetPort = 23
	defaultJobs       = 1
	connectTimeout    = 15 * time.Second
	retryDelay        = 5 * time.Second
	retryCount        = 1
)

var (
	// универсальный промпт — покрывает Cisco/JunOS/Huawei/MikroTik/Eltex/D-Link
	// исключает JunOS diff строки и маршруты вида "via 1.2.3.4 >"
	promptRE = regexp.MustCompile(`(?m)^[\w<\[][^\n]{0,62}(\][>\s]*[>#$]|[^\s][>#$])\s*$`)
	passRE   = regexp.MustCompile(`(?i)assword:`)
	loginRE  = regexp.MustCompile(`(?im)(login|username|user)\s*:\s*$`)
	// пагинация — все популярные варианты
	moreRE = regexp.MustCompile(`(?i)-+\s*more\s*-+|\[more [0-9]+%\]|<more>`)
	// ANSI escape коды (MikroTik и другие)
	ansiRE = regexp.MustCompile(`\x1B\[[\x30-\x3F]*[\x20-\x2F]*[\x40-\x7E]|\x1B[()][AB012]`)
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

// spawnCmd возвращает строку запуска в зависимости от протокола
func spawnCmd(cfg Config, device string) string {
	switch cfg.Proto {
	case ProtoTelnet:
		return fmt.Sprintf("telnet %s %d", device, cfg.Port)
	default:
		return fmt.Sprintf(
			"ssh -o StrictHostKeyChecking=no -o CheckHostIP=no -o ConnectTimeout=%d -p %d -l %s %s",
			int(connectTimeout.Seconds()), cfg.Port, cfg.Username, device,
		)
	}
}

// connectAndRun подключается к устройству и выполняет команды
func connectAndRun(device string, cfg Config) (string, error) {
	e, _, err := expect.Spawn(
		spawnCmd(cfg, device),
		connectTimeout,
		expect.Verbose(cfg.Debug),
		expect.VerboseWriter(os.Stderr),
	)
	if err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	defer e.Close()

	// для telnet: login → username → password → prompt
	// для ssh: сразу prompt или password
	if cfg.Proto == ProtoTelnet {
		_, _, idx, err := e.ExpectSwitchCase([]expect.Caser{
			&expect.Case{R: loginRE, T: expect.OK()},
			&expect.Case{R: promptRE, T: expect.OK()},
		}, cfg.Timeout)
		if err != nil {
			return "", fmt.Errorf("telnet login prompt: %w", err)
		}
		// отправляем username только если поймали loginRE (idx=0)
		// idx=1 означает устройство сразу дало промпт — username не нужен
		if idx == 0 {
			if err = e.Send(cfg.Username + "\r\n"); err != nil {
				return "", fmt.Errorf("telnet send username: %w", err)
			}
		}
	}

	// ждём пароль или промпт
	_, _, _, err = e.ExpectSwitchCase([]expect.Caser{
		&expect.Case{
			R:  passRE,
			S:  cfg.Password + "\r\n",
			T:  expect.Continue(expect.NewStatus(codes.PermissionDenied, "wrong password")),
			Rt: 2,
		},
		&expect.Case{R: promptRE, T: expect.OK()},
	}, cfg.Timeout)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}

	// отправляем пустую строку чтобы получить свежий промпт
	// и гарантированно сбросить буфер после логина/MOTD
	if err = e.Send("\r\n"); err != nil {
		return "", fmt.Errorf("send empty: %w", err)
	}
	if _, _, err = e.Expect(promptRE, cfg.Timeout); err != nil {
		return "", fmt.Errorf("prompt after login: %w", err)
	}

	var buf strings.Builder

	for _, cmd := range cfg.Commands {
		if err := e.Send(cmd + "\r\n"); err != nil {
			return buf.String(), fmt.Errorf("send %q: %w", cmd, err)
		}
		// ждём эхо команды — это гарантирует что устройство
		// начало обрабатывать команду и буфер обновился
		if _, _, err = e.Expect(regexp.MustCompile(regexp.QuoteMeta(cmd)), cfg.Timeout); err != nil {
			return buf.String(), fmt.Errorf("echo %q: %w", cmd, err)
		}
		// ждём промпт обрабатывая пагинацию
		for {
			result, _, _, matchErr := e.ExpectSwitchCase([]expect.Caser{
				&expect.Case{R: moreRE, S: " ", T: expect.Continue(expect.NewStatus(codes.OK, "more"))},
				&expect.Case{R: promptRE, T: expect.OK()},
			}, cfg.Timeout)
			if matchErr != nil {
				return buf.String(), fmt.Errorf("expect after %q: %w", cmd, matchErr)
			}
			buf.WriteString(stripANSI(result))
			if promptRE.MatchString(result) {
				break
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

	// горутина-сборщик — единственный читатель resultsCh, блокировка не нужна
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
