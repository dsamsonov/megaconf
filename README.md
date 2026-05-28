# megaconf v2.2

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
Usage: megaconf [-?drpvT] [-c value] [-C value] [-D value] [-h value] [-j value] [-J value] [-l value] [-P value] [-t value] [-u value]
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
 -l, --log=value         combined log file (output goes to stdout AND log file)
 -J, --json-log=value    write results to a JSON file (keyed by device)
 -D, --log-dir=value     directory: one log file per device (<name>.log)
 -T, --telnet            use Telnet instead of SSH (default: SSH)
 -r, --run               run commands (required)
 -d, --debug             debug mode (forces -j 1 for readable output)
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

# Save combined output to log file (stdout is also printed)
megaconf -r -u admin -p -l ./output.log

# Structured results as JSON (keyed by device)
megaconf -r -u admin -p -J ./results.json

# One log file per device into a directory
megaconf -r -u admin -p -D ./logs

# Inline command
megaconf -r -u admin -p -C "sh ver"

# Parallel execution on 10 devices at once
megaconf -r -u admin -p -j 10

# Custom hosts and commands files
megaconf -r -u admin -p -h ./my_devices.db -c ./my_commands

# Custom timeout
megaconf -r -u admin -p -t 120
```

## Logging

Output **always** goes to stdout. On top of that, three independent log sinks can be
combined in any way:

- `-l <file>` — combined log: the same blocks you see on stdout, written to one file.
- `-J <file>` — JSON document keyed by device name. Each entry has `result`
  (`success`/`unsuccess`), `error` (present only on failure), and `out`
  (session output, always present). Written once after all devices finish.
- `-D <dir>` — one file per device, named `<device>.log`, containing that device's
  block. The directory is created if missing.

JSON example:
```json
{
  "juniper1":    { "result": "success", "out": "..." },
  "192.168.0.1": { "result": "unsuccess", "error": "login: connection closed [...]", "out": "" }
}
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
| macOS   | ✓     |     | ✓     |     |
| FreeBSD | ✓     |     | ✓     |     |

Binaries are statically linked — no dependencies required on target system.

## SSH

- Uses system `ssh` binary — all `~/.ssh/config` settings apply automatically
- SSH agent forwarding via `SSH_AUTH_SOCK`
- `StrictHostKeyChecking=no` and `UserKnownHostsFile=/dev/null` — intentional, network
  hardware keys change after firmware updates and must not break password auth
- Password auth via `-p` works at the SSH auth layer through `SSH_ASKPASS` (the binary
  acts as its own askpass helper). This requires **OpenSSH ≥ 8.4** on the client. With
  older clients use keys/agent, or rely on devices that prompt for a password inside the
  session. Not enabled on Windows.

## Telnet

- Uses system `telnet` binary
- Username from `-u`, password from `-p`
- Same credentials for all devices

## Notes

- One password for all devices by design
- Output always goes to stdout; `-l`/`-J`/`-D` add files in parallel
- On Ctrl+C the context is cancelled: active SSH/Telnet sessions are killed, the log file
  is flushed and closed, and a summary of what completed is still printed (exit code 130)
- Connection retried once (after 5s) only on transport errors, never on auth failures
- Failed devices appear in the Unsuccessful section with reason (including the tail of the
  ssh/telnet stderr, e.g. "Connection refused")
- Pagination (`---- More ----`, `---(more)---`, `[more 50%]`, `<more>`) handled automatically
- ANSI escape codes stripped from output (MikroTik etc.)
