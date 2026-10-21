# logviewer

A lightweight, SSH-based log tailing console for a small fleet of servers.

No ELK, no agents, no log shipping. The binary connects to each host over SSH,
streams `tail -F` output to the browser over WebSocket, and lets you switch
between hosts, applications, and arbitrary files from one page.

> Intended for an isolated internal network. There is **no authentication** and
> host passwords are stored in plain text in `config.yaml`. Do not expose it to
> the internet.

## Features

- **Three ways to read logs**
  - *Host logs* — browse a host's log directory, pick a file, tail it.
  - *Application logs* — predefined application entries, one column per host,
    side by side, for correlating the same service across machines.
  - *Path* — paste any absolute path and tail it immediately (recent paths are
    remembered in the browser).
- Live tail with pause / resume, per-pane download, and a full-screen view.
- Content filter with highlighted matches; `ERROR` / `WARN` lines get colour.
- Scroll to the top to page in older lines without losing your position.
- Single static binary: the front-end is embedded with `go:embed`.
- Hosts are declared once and referenced by nickname; app entries list the
  exact `host:path` pairs, so there is no guessing of file names.

## Quick start

```bash
# 1. build (from a machine with Go; cross-compile for the target)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o logviewer .

# 2. configure
cp config.example.yaml config.yaml
vi config.yaml

# 3. run
./start.sh                 # writes logviewer.pid, logs to logviewer.log
# or: ./logviewer -config config.yaml
```

Then open `http://<server>:23344/`. The port comes from `config.yaml`.

To stop, run `./stop.sh`, which only touches the process started from the
same directory (verified through `/proc/<pid>/cwd`).

## Configuration

`config.yaml` is YAML, with application entries written as plain blocks:

```yaml
port: 23344
tail_lines: 200            # lines loaded on first open
allow_any_path: true       # the Path tab may open any absolute path

defaults:
  user: root
  port: 22
  dir: /var/log/app

hosts:                     # declared once, referenced by nickname
  - web1: {ip: 10.0.0.11, label: Web 1, pass: 'change-me'}
  - web2: {ip: 10.0.0.12, label: Web 2, pass: 'change-me'}

apps: |
  gateway 网关[
  web1:/var/log/app/gateway.log
  web2:/var/log/app/gateway.log
  ]

  worker[
  web1:/var/log/app/worker.log
  web2:/var/log/app/worker.log
  ]
```

Inside `apps`, each block is:

```
application-id [display name][
host-nickname:/absolute/path/on/that/host
...
]
```

Block order is the dropdown order; line order is the column order. Paths must be
absolute and are used verbatim. A missing `]`, a line without `host:path`, a
relative path, or an unknown host aborts startup with the offending line number.

`allow_any_path: false` restricts reads to the configured directories and files
plus an optional allow-list:

```yaml
allow_any_path: false
path_roots:
  - /var/log
  - /data/logs
```

Verify what the server actually parsed with:

```bash
./logviewer -config config.yaml -check
```

It prints hosts and resolved sources as JSON (passwords are never printed).

## HTTP endpoints

| Path | Purpose |
| --- | --- |
| `/` | Host log browser |
| `/app.html` | Application view |
| `/tail.html` | Arbitrary path view |
| `/ws` | WebSocket: `init`, `load_prev`, `stop`, `list_dir` |
| `/api/hosts` | Host list |
| `/api/apps` | Application list with resolved sources |
| `/api/stat` | Existence / size / mtime of a remote path |
| `/api/download` | Stream a whole file |

## Requirements

- Go 1.21+ to build; the binary itself only needs Linux (or any OS Go targets).
- Targets must accept password SSH authentication for the configured user.
- `find`, `tail` and `stat` must exist on the targets (standard on Linux).

## Layout

```
main.go           config parsing, HTTP routes, host/app resolution
ssh_handler.go    SSH pool, directory listing, tail, stat, download
websocket.go      WebSocket session: init / load_prev / stop / list_dir
static/index.html host log browser
static/app.html   application view (one column per host)
static/tail.html  arbitrary path view
config.example.yaml
start.sh, stop.sh
```

## License

Apache-2.0. See [LICENSE](LICENSE).
