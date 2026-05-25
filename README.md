# megaconf v2.0

Utility for fast execution of commands on many network devices (routers, switches, servers, etc.)

## Installation

Download binary from https://github.com/dsamsonov/megaconf/releases

Or build from source:
```bash
go mod tidy
make build
```

## Usage

```
Usage: megaconf [-?drpvT] [-c value] [-C value] [-h value] [-j value] [-l value] [-P value] [-t value] [-u value]
 -?, --help              display help
 -v, --version           display version
 -h, --hosts=value       file with devices list [./devices.db]
 -c, --cmdlist=value     file with commands list (mutually exclusive with --cmd)
 -C, --cmd=value         inline command, e.g. -C "sh ver" (mutually exclusive with --cmdlist)
 -u, --username=value    username (default: current OS user)
 -p, --password          prompt for password
 -j, --jobs=value        number of parallel device jobs [1]
 -t, --timeout=value     timeout in seconds (connect + command) [60]
 -P, --port=value        port (default: 22 for SSH, 23 for Telnet)
 -l, --log=value         log file (output goes to stdout AND log file)
 -T, --telnet            use Telnet instead of SSH (default: SSH)
 -r, --run               run commands (required)
 -d, --debug             debug mode
```

## File formats

**devices.db**
```
# Lines starting with # are ignored
# One device per line: hostname or IP

# JunOS routers
juniper1
juniper2

# Core switches
192.168.0.1
192.168.0.2
```

**commands**
```
# Comments work here too
sh version
show chassis routing
sh int description | no-more
```

## Examples

```bash
# Run commands via SSH (default)
megaconf -r -u admin -p

# Run commands via Telnet
megaconf -r -u admin -p --telnet

# Telnet on non-standard port
megaconf -r -u admin -p --telnet -P 2023

# Save output to log file (stdout is also printed)
megaconf -r -u admin -p -l ./output.log

# Inline command
megaconf -r -u admin -p -C "sh ver"

# Parallel execution on 10 devices at once
megaconf -r -u admin -p -j 10

# Custom hosts and commands files
megaconf -r -u admin -p -h ./my_devices.db -c ./my_commands

# Custom timeout
megaconf -r -u admin -p -t 120
```

## Build

```bash
make build    # binary for current platform
make all      # binaries for all platforms + tgz package
make clean    # remove build artifacts
```

Supported platforms:

| OS    | amd64 | 386 | arm64 | arm |
|-------|-------|-----|-------|-----|
| Linux | ✓     | ✓   | ✓     | ✓   |
| macOS | ✓     |     | ✓     |     |

Binaries are statically linked — no dependencies required on target system.

## SSH

- Uses system `ssh` binary — all `~/.ssh/config` settings apply automatically
- SSH agent forwarding via `SSH_AUTH_SOCK`
- Password auth via `-p` as fallback
- `StrictHostKeyChecking=no` — intentional, network hardware keys change after firmware updates

## Telnet

- Uses system `telnet` binary
- Username from `-u`, password from `-p`
- Same credentials for all devices

## Notes

- One password for all devices by design
- Output always goes to stdout; `-l` adds a log file in parallel
- On Ctrl+C all active sessions are aborted immediately
- Failed devices appear in Unsuccessful section with reason
- Pagination (`---- More ----`, `[more 50%]`, `<more>`) handled automatically
- ANSI escape codes stripped from output (MikroTik etc.)
