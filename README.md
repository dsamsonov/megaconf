# megaconf v2.4

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
Usage: megaconf [-?drpvST] [-M] [--live] [-c value] [-C value] [-D value] [-h value] [-j value] [-J value] [-l value] [-P value] [-t value] [-u value] [--char-delay value] [--login-user value] [--login-pass]
 -?, --help              display help
 -v, --version           display version
 -h, --hosts=value       file with devices list [./devices.db]
 -c, --cmdlist=value     file with commands list (mutually exclusive with --cmd)
 -C, --cmd=value         inline command, e.g. -C "sh ver" (mutually exclusive with --cmdlist)
 -u, --username=value    username (default: current OS user)
 -p, --password          prompt for password
 -j, --jobs=value        number of parallel device jobs [1]
 -t, --timeout=value     timeout in seconds (connect + command) [60]
 -P, --port=value        default port for lines without one (default: 22 SSH / 23 Telnet)
 -l, --log=value         combined log file (output goes to stdout AND log file)
 -J, --json-log=value    write results to a JSON file (keyed by device)
 -D, --log-dir=value     directory: one log file per device (<name>.log)
 -T, --telnet            use Telnet instead of SSH (default: SSH)
 -S, --slow              slow paste mode: send input char-by-char (for slow console servers)
     --char-delay=value  inter-character delay in ms for --slow [30]
 -M, --console           console-server mode (Moxa etc.): in-session device login after connect; forces -j 1
     --login-user=value  device login username for --console (default: --username)
     --login-pass        prompt for a separate device login password for --console
     --live              stream session output live as it happens (forces -j 1; implied by --console)
 -r, --run               run commands (required)
 -d, --debug             debug mode (forces -j 1 for readable output)
```

## File formats

**devices.db**
```
# Lines starting with # are ignored
# One device per line: hostname or IP, with an optional :port

# JunOS routers
juniper1
juniper2

# Core switches
192.168.0.1
192.168.0.2

# Per-device port (overrides -P / the default):
#   host:port, ipv4:port, or [ipv6]:port
console-srv.lab:4001
10.0.0.5:2222
[2001:db8::10]:4002
```

Port precedence per line: an explicit `:port` in the line wins, otherwise the `-P`
value is used, otherwise the protocol default (22 for SSH, 23 for Telnet). A bare
IPv6 address without a port must be written **without** brackets (`2001:db8::1`);
to attach a port, bracket it (`[2001:db8::1]:4001`).

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

# Slow paste through a slow console server (e.g. Moxa @9600)
megaconf -r -u admin -p -S

# Slow paste with a custom inter-character delay (50 ms)
megaconf -r -u admin -p -S --char-delay 50

# Console server (Moxa) over SSH: SSH auth = Moxa account (-u/-p),
# device login behind it = --login-user / --login-pass. -M implies -j 1 and live.
megaconf -r -u admin -p -M --login-user noc --login-pass

# Console server reachable as raw telnet port (one login = the device itself)
megaconf -r -T -P 4001 -u noc -p -M

# Console server + slow paste (typical for serial-attached gear at 9600)
megaconf -r -u admin -p -M --login-user noc --login-pass -S

# Watch the session live as it happens (plain SSH, no console server)
megaconf -r -u admin -p --live

# Per-device ports live in devices.db (host:port); no flag needed
megaconf -r -u admin -p   # devices.db may contain console-srv:4001 etc.

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
- Password auth via `-p`: `ssh` is run attached to a **pseudo-terminal (PTY)**, exactly
  like an interactive login, and megaconf answers the `password:` prompt itself. This is
  the same technique `sshpass` uses, and it works on **all** OpenSSH versions — including
  old clients such as OpenSSH 7.4 on RHEL/CentOS 7 — with no `SSH_ASKPASS`/`DISPLAY`
  tricks. The PTY also means the remote session is a real interactive terminal, which is
  what console servers (Moxa etc.) need in order to produce their banner/login prompt.
- The PTY is unix-only. On **Windows** megaconf falls back to plain pipes and password
  auth is not supported there — use keys/agent.

> Note: giving `ssh` a real terminal (rather than a pipe + `SSH_ASKPASS`) is what makes
> serial consoles behind a Moxa actually talk; with a bare pipe many of them stay silent.

## Telnet

- Uses system `telnet` binary
- Username from `-u`, password from `-p` (entered in-session)
- Same credentials for all devices
- Per-device ports from `devices.db` apply (`host:4001`), as does `-P`
- Works with console servers too: a raw telnet port that maps straight onto a serial
  line lands directly at the **device** login, so plain `-T` already performs an
  in-session login with `-u`/`-p`. Add `-M` to also get the "quiet line" nudge and
  fast-fail on bad credentials (see below). `-S` is honored over Telnet as well — the
  system telnet client forwards the throttled bytes without re-batching them.
- Line endings over Telnet are governed by the telnet client's NVT handling; the
  console-mode single-CR is effectively a no-op there but does no harm.

## Slow paste (`-S`)

When pushing configuration through a slow console/terminal server (for example a Moxa
serial server with the device port at 9600 bps), characters sent in a burst can be
dropped. The `-S` / `--slow` flag sends **all input** — commands, any in-session password
(Telnet, enable, or the `-M` device login), and the pagination space — one byte at a time
with a delay between characters, controlled by `--char-delay <ms>` (default 30).

- Under `-S`, every byte megaconf types is throttled — including the password it enters
  at an `ssh` or device `password:` prompt (harmless: the SSH password reaches the server
  over TCP, not the slow serial line). Key/agent auth involves no typing and is unaffected.
- The slow send is interrupted cleanly on `Ctrl+C` / timeout.
- A long line at a high delay can take a while (e.g. 2000 chars × 30 ms ≈ 60 s); the
  command timeout `-t` is measured separately, while waiting for the prompt after the
  line is sent.

## Console servers / Moxa (`-M`)

A console (terminal) server such as a Moxa exposes each serial port as a TCP endpoint.
Reaching the attached device is a **two-stage** affair, and the two stages usually have
**different credentials**:

1. **Transport** — you connect to the console server itself.
   - Over **SSH**: the SSH login authenticates the *console-server* account (`-u` / `-p`).
   - Over **Telnet** to a raw port-mapped endpoint: there is no server-level login; you
     land straight on the device.
2. **Device login** — the serial console then runs its own `login:` / `Password:`
   dialog for the *device* behind the port.

`-M` / `--console` turns on stage 2: after the transport is up, megaconf performs the
in-session device login. The device credentials are separate from the transport ones:

- `--login-user <name>` — device username (defaults to `-u` if omitted).
- `--login-pass` — prompt for a separate device password (if omitted, the `-p` password
  is reused for the device login as well).

So an SSH-fronted Moxa where the SSH account is `admin` and the device login is `noc`:

```bash
megaconf -r -u admin -p -M --login-user noc --login-pass
#         └ SSH auth (Moxa) ┘  └ device login behind the port ┘
```

What `-M` adds on top of a plain login:

- **Nudge for quiet lines** — a serial console often stays silent until it sees a
  keystroke. `-M` sends a single Enter to wake the line, retrying a few times before
  giving up, so a dormant port still reaches its `login:`.
- **Fast-fail on bad credentials** — if the device answers with `Login incorrect` /
  `% login invalid` / `access denied` etc., megaconf stops immediately and marks the
  device failed **without** retrying — important on consoles that lock an account after
  a few bad attempts.
- **Serial-friendly line ending** — a single `CR` is used instead of `CR LF`.
- **Implies `-j 1` and `--live`** — console servers usually allow one session per port,
  and serial work benefits from watching it happen.

`-M` works over Telnet too (raw port → device login); there the extra value is the
nudge and fast-fail. Combine with `-S` for slow serial lines:

```bash
megaconf -r -u admin -p -M --login-user noc --login-pass -S
```

### Logging out of the device

There is no logout flag — just put the logout command **last** in your command list:

```
show version
sh int description | no-more
exit
```

When the final command closes the session (`exit` / `logout` / `reload`), the resulting
connection close is treated as a **normal, successful** end of session — not an error.
(An unexpected disconnect in the *middle* of the command list is still reported as a
failure.) If you do not log out, megaconf simply drops the session when done.

## Live output (`--live`)

By default each device's output is printed as one complete block once that device
finishes (so parallel runs never interleave). `--live` instead streams the raw session
to **stdout as it happens** — handy for watching a long push on a slow console. It is
implied by `-M`.

- `--live` forces `-j 1` (one device at a time) so the live stream stays readable.
- The live stream on stdout is the raw session (device echo and all). The file sinks
  stay clean: `-l` receives the same tidy per-device block as a normal run, and `-J` /
  `-D` are unaffected. On failure the error is also surfaced on stdout.
- Passwords typed during an in-session/device login are hidden by the device's own echo
  suppression, exactly as on a real terminal.

## Notes

- One password for all devices by design
- Output always goes to stdout; `-l`/`-J`/`-D` add files in parallel
- On Ctrl+C the context is cancelled: active SSH/Telnet sessions (and their child
  processes) are killed by process group, the log file is flushed and closed, and a
  summary of what completed is still printed (exit code 130)
- Connection retried once (after 5s) only on transport errors, never on auth failures
  (including the in-session device login failure under `-M`)
- A session that closes right after the **last** command (logout/reload) counts as
  success; an unexpected close earlier is a failure
- Per-device ports are read from `devices.db` (`host:port`); `-P` sets the default for
  lines without one
- Failed devices appear in the Unsuccessful section with reason (including the tail of the
  ssh/telnet stderr, e.g. "Connection refused")
- Pagination (`---- More ----`, `---(more)---`, `[more 50%]`, `<more>`) handled automatically
- ANSI escape codes stripped from output (MikroTik etc.)
