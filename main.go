package main
import ( 
    "fmt"
    "os"
    "bufio"
    "log"
    "time"
    "strings"
    "regexp"
    "strconv"
    "github.com/pborman/getopt/v2"
    "github.com/google/goexpect"
    "github.com/howeyc/gopass"
    "google.golang.org/grpc/codes"
)

const (
    version="0.0.1"
)

func Fatal (err error) {
    fmt.Printf("\nERROR! %s\n\n",err)
    os.Exit(1)
}
        
func ReadFile (file string) []string {
    f, err := os.Open(file)
    if err != nil {
	Fatal(err)
    }
    defer f.Close()
    out := make([]string,0)
    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
	line:=strings.TrimSpace(scanner.Text())
        if len(line)!=0 { out=append(out,line) }
    }
    if err := scanner.Err(); err != nil {
        Fatal(err)
    }
    return out
}

func main() {
    var (
	devices,commands []string
	password string
    )
    //parse command arguments
    optHelp := getopt.BoolLong("help",'?', "display help")
    optVersion := getopt.BoolLong("version",'v', "display version")
    optDevFile := getopt.StringLong("hosts", 'h', "./devices.db", "File with devices list")
    optCmdFile := getopt.StringLong("cmdlist", 'c', "./commands", "File with commands list")
    optTimeout := getopt.StringLong("timeout", 't',"60","Timeout in seconds")
    optPassword  := getopt.BoolLong("password",'p',"Promt for password")
    optRun     := getopt.BoolLong("run",'r',"Run commands")
    optDebug   := getopt.BoolLong("debug",'d',"Debug mode")
//    optLogFile := getopt.StringLong("log", 'l', "./megaconf.log", "Log file")
    getopt.Parse()
    if *optHelp { getopt.Usage(); os.Exit(0) }
    if *optVersion { fmt.Println(version); os.Exit(0) }
    if *optRun!=true { getopt.Usage(); os.Exit(0) }
    //read files
    devices=ReadFile(*optDevFile)
    commands=ReadFile(*optCmdFile)
    if len(devices)==0 { Fatal(fmt.Errorf("File %s dont be empty",*optDevFile)) }
    if len(commands)==0 { Fatal(fmt.Errorf("File %s dont be empty",*optCmdFile)) }
    if *optPassword==true {
	fmt.Printf("Enter password: ")
	p,err:=gopass.GetPasswd()
	if err != nil { Fatal(err) }
	password=string(p)
    }
    // connect to devices
    s,_:=strconv.Atoi(*optTimeout)
    timeout:=time.Duration(s) * time.Second
    promptRE:= regexp.MustCompile("(>|#|\\$|>\\s|])$")
    passRE:= regexp.MustCompile("assword:")
    for di:=0; di<len(devices); di++ {
	fmt.Printf("\n\n##############################################\n#    Connecting to %s, [%d/%d]\n##############################################\n\n\n",devices[di],di+1,len(devices))
	e, _, err := expect.Spawn(fmt.Sprintf("ssh -o StricthostKeyChecking=no -o CheckHostIP=no -p 22 %s",devices[di]),-1,expect.Verbose(*optDebug))
        if err != nil { log.Fatal(err) }
        defer e.Close()
	_, _, _, err=e.ExpectSwitchCase([]expect.Caser{
	    &expect.Case{R: passRE,S: password+"\n", T: expect.Continue(expect.NewStatus(codes.PermissionDenied, "Access denied, wrong password")), Rt: 2},
	    &expect.Case{R: promptRE,T: expect.OK()},
	},timeout)
	if err!=nil {
	    log.Fatal(err)
	    continue
	}
	//e.Expect(promptRE, timeout)
        // run commands
        for i:=0; i<len(commands); i++ {
            e.Send(commands[i]+"\n\r")
            result, _, _:= e.Expect(promptRE, timeout)
	    fmt.Println(result)
	}
    }
}	
