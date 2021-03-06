# megaconf
Utility for fast run list of commands to many network hardware (routers,switches, servers, etc)

## Installation
download binary from https://github.com/dsamsonov/megaconf/releases or git clone and compile this repositiry

##Usage
```bash
Usage: megaconf [-?dprv] [-c value] [-h value] [-j value] [-l value] [-P value] [-t value] [-u value] [parameters ...]
 -?, --help         display help
 -c, --cmdlist=value
                    file with commands list [./commands]
 -d, --debug        debug mode
 -h, --hosts=value  file with devices list [./devices.db]
 -j, --jobs=value   number of parallel device jobs [1]
 -l, --log=value    Log file
 -p, --password     promt for password
 -P, --port=value   connect to port [22]
 -r, --run          run commands
 -t, --timeout=value
                    timeout in seconds [60]
 -u, --username=value
                    Username
 -v, --version      display version

by default output to console, if you want redirect output to file, use -l flag
-r serves to protect against accidental startup. If you want run commands on your hardware, you need to specify this flag
```
```
format device file:
<hostname1>
<hostname2>
or:
<ip addr1>
<ip addr2>

format commands file:
cmd1
cmd2
cmd3
```

Only ssh supported at this moment, if your need telnet or mikrotik-api, think deeply if you need it. And if necessary, then write to me about it
