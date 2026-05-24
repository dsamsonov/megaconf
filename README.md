# megaconf v2.0

Utility for fast execution of commands on many network devices (routers, switches, servers, etc.)

## Installation

Download binary from https://github.com/dsamsonov/megaconf/releases

Or build from source:
```bash
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
 -t, --timeout=value     timeout in seconds for connect and commands [60]
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

juniper1
juniper2
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

| OS      | amd64 | 386 | arm64 | arm |
|---------|-------|-----|-------|-----|
| Linux   | ✓     | ✓   | ✓     | ✓   |
| FreeBSD | ✓     | ✓   | ✓     |     |
| macOS   | ✓     |     | ✓     |     |
| Windows | ✓     | ✓   | ✓     |     |

Binaries are statically linked — no dependencies required on target system.
Windows binaries have `.exe` extension.

## Notes

- SSH is the default protocol; use `--telnet` / `-T` to switch
- Telnet auth: username from `-u`, password from `-p` (same for all devices)
- SSH config is read from `~/.ssh/config` as usual
- `StrictHostKeyChecking=no` is intentional — network hardware keys change after firmware updates
- One password for all devices by design — use SSH keys via `~/.ssh/config` for per-device auth
- Output always goes to stdout; `-l` adds a log file in parallel
