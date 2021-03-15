package main

import (
	"bufio"
	"fmt"
	"github.com/google/goexpect"
	"github.com/howeyc/gopass"
	"github.com/pborman/getopt/v2"
	"github.com/zenthangplus/goccm"
	"google.golang.org/grpc/codes"
	"log"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"
)

var (
	promptRE = regexp.MustCompile("(>|#|\\$|>\\s|])$")
	passRE   = regexp.MustCompile("assword:")
	timeout  time.Duration
)

func Fatal(err error) {
	fmt.Printf("\nERROR! %s\n\n", err)
	os.Exit(1)
}

func ReadFile(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		Fatal(err)
	}
	defer f.Close()
	out := make([]string, 0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) != 0 {
			out = append(out, line)
		}
	}
	if err := scanner.Err(); err != nil {
		Fatal(err)
	}
	return out
}

func CmdToDevice(c goccm.ConcurrencyManager, device string, optDebug *bool, username, password string, commands []string, port int) {
	defer c.Done()
	e, _, err := expect.Spawn(fmt.Sprintf("ssh -o StricthostKeyChecking=no -o CheckHostIP=no -p %d -l %s %s", port,username, device), -1, expect.Verbose(*optDebug), expect.VerboseWriter(os.Stdout))
	if err != nil {
		log.Printf("device %s, error: %s\n", device, err)
		return
	}
	defer e.Close()
	_, _, _, err = e.ExpectSwitchCase([]expect.Caser{
		&expect.Case{R: passRE, S: password + "\n", T: expect.Continue(expect.NewStatus(codes.PermissionDenied, "Access denied, wrong password")), Rt: 2},
		&expect.Case{R: promptRE, T: expect.OK()},
	}, timeout)
	if err != nil {
		log.Printf("device %s, error: %s\n", device, err)
		return
	}
	//e.Expect(promptRE, timeout)
	// run commands
	for i := 0; i < len(commands); i++ {
		err = e.Send(commands[i] + "\n\r")
		if err != nil {
			log.Printf("device %s, error while sending command \"%s\": %s\n", device, commands[i], err)
			return
		}
		result, _, err := e.Expect(promptRE, timeout)
		if err != nil {
		        log.Printf("device %s, error after sending command \"%s\": %s\n", device, commands[i], err)
    			return
		}
		log.Printf("device %s, result: %s\n", device, result)
	}
}

func main() {
	const (
		version = "0.0.2"
	)

	var (
		devices, commands []string
		username,password string
	)

	//parse command arguments
	optHelp := getopt.BoolLong("help", '?', "display help")
	optVersion := getopt.BoolLong("version", 'v', "display version")
	optDevFile := getopt.StringLong("hosts", 'h', "./devices.db", "file with devices list")
	optCmdFile := getopt.StringLong("cmdlist", 'c', "./commands", "file with commands list")
	optUsername := getopt.StringLong("username", 'u', "", "Username")
	optJobs := getopt.IntLong("jobs", 'j', 1, "number of parallel device jobs")
	optTimeout := getopt.IntLong("timeout", 't', 60, "timeout in seconds")
	optPort := getopt.IntLong("port", 'P', 22, "connect to port")
	optPassword := getopt.BoolLong("password", 'p', "promt for password")
	optRun := getopt.BoolLong("run", 'r', "run commands")
	optDebug := getopt.BoolLong("debug", 'd', "debug mode")
	optLogFile := getopt.StringLong("log", 'l', "", "Log file")
	getopt.Parse()
	if *optHelp {
		getopt.Usage()
		os.Exit(0)
	}
	if *optVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if *optRun != true {
		fmt.Println("\nIf you want to run commands on devices, use flag -r\n")
		getopt.Usage()
		os.Exit(0)
	}
	if *optUsername == "" {
		currentUser, err := user.Current()
		if err != nil {
		    Fatal(err)
		}
    		username = currentUser.Username
	} else {
		username = *optUsername
        }
	//read files
	devices = ReadFile(*optDevFile)
	commands = ReadFile(*optCmdFile)
	if len(devices) == 0 {
		Fatal(fmt.Errorf("file %s dont be empty", *optDevFile))
	}
	if len(commands) == 0 {
		Fatal(fmt.Errorf("file %s dont be empty", *optCmdFile))
	}
	if *optPassword == true {
		fmt.Printf("Enter password: ")
		p, err := gopass.GetPasswd()
		if err != nil {
			Fatal(err)
		}
		password = string(p)
	}
	timeout = time.Duration(*optTimeout) * time.Second
	if *optLogFile != "" {
		lf, err := os.Create(*optLogFile)
		if err != nil {
			Fatal(err)
		}
		log.SetOutput(lf)
		defer lf.Close()
	}
	// connect to devices
	c := goccm.New(*optJobs)

	for di := 0; di < len(devices); di++ {
		c.Wait()
		fmt.Printf("\n\n##############################################\n#    Connecting to %s, [%d/%d]\n##############################################\n\n\n", devices[di], di+1, len(devices))
		go CmdToDevice(c, devices[di], optDebug, username, password, commands, *optPort)
	}
	c.WaitAllDone()
}
