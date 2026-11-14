# disk-hog

Find and reclaim disk space wasted by development tooling: `node_modules`,
virtualenvs, `target/` and `build/` folders, package-manager caches, old SDK
and toolchain versions, VM images and models, editor caches, crash dumps,
huge logs, duplicates and large files. It also suggests system-level
cleanups (package caches, the systemd journal, old kernels, snap
revisions) and prunes Docker or Podman storage.

- **One static binary.** Pure Go with no dependencies and no cgo, so it
  runs on any Linux distribution and architecture (glibc or musl), plus
  macOS, Windows and FreeBSD.
- **Fast and light.** It walks folders in parallel. A home folder of 135 GB
  and 560k files scans in about 1–3 s. The service idles at about 15 MB of
  RAM and runs with low CPU and I/O priority.
- **Safe by design.** Every item has a risk level and a note on how to
  regenerate it. Items go to the system trash by default unless they
  rebuild automatically. Each path is checked again just before removal.
  The browser can only refer to scan results by ID, never by path.
- **Dashboard and CLI.** The dashboard is at http://localhost:3002. Every
  feature also works from the terminal for servers and scripts.

## Install

### Requirements

- Linux (any distribution or architecture), macOS or FreeBSD. Windows is
  supported too; see [Windows](#windows).
- `git`, to clone the repository.
- Go 1.21 or newer, to build from source. If a machine has no Go, see
  [Machines without Go](#machines-without-go).

Install Go from your package manager if it isn't there yet:

| System | Command |
|---|---|
| Debian, Ubuntu, Mint | `sudo apt install golang-go` |
| Fedora, RHEL | `sudo dnf install golang` |
| Arch, Manjaro | `sudo pacman -S go` |
| openSUSE | `sudo zypper install go` |
| Alpine | `sudo apk add go` |
| macOS | `brew install go` |

Some LTS releases ship a Go older than 1.21. Check with `go version`; if it
is too old, install the latest from https://go.dev/dl.

### Clone and deploy

```sh
git clone https://github.com/kishore-144/disk-hog.git
cd disk-hog
./deploy.sh
```

To clone over SSH instead, use
`git clone git@github.com:kishore-144/disk-hog.git`.

When it finishes, open **http://localhost:3002**. You don't need root:
everything is installed for your user only.

The script runs these steps in order:
1. Runs `git pull --ff-only` to fetch the latest version.
2. Builds the binary.
3. Installs it to `~/.local/bin/disk-hog`.
4. Starts (or restarts) the background service.
5. Checks that the service responds.

### Update

From the folder you cloned into, run the same command again:

```sh
cd disk-hog
./deploy.sh
```

### Options

| Option | What it does |
|---|---|
| `--port 4000` | Serve on another port (default 3002). |
| `--system` | Install the binary to `/usr/local/bin` (uses sudo or doas). |
| `--linger` | Keep running after logout (systemd `enable-linger`). |
| `--no-pull`, `--no-build`, `--no-service` | Skip that step. |
| `status`, `logs`, `restart`, `stop` | Manage the running service. |
| `uninstall [--purge]` | Remove the service and binary. `--purge` also removes config and history. |

`make deploy` does the same as `./deploy.sh`.

If the script warns that `~/.local/bin` is not on your `PATH`, add this line
to your shell profile (`~/.bashrc`, `~/.zshrc` or `~/.profile`) and open a
new terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

**Service manager.** The script picks whatever the machine has:
- a systemd **user** service on most distributions;
- a launchd agent on macOS;
- otherwise (Alpine/OpenRC, Void/runit, containers, WSL without systemd) a
  background process, plus a desktop autostart entry or an `@reboot` cron
  hint.

### Machines without Go

Build release archives on any machine that has Go:

```sh
git clone https://github.com/kishore-144/disk-hog.git
cd disk-hog
make dist
```

This writes `dist/disk-hog-<os>-<arch>.tar.gz` for 16 platforms. Copy the
archive that matches the target machine (`uname -m` shows its
architecture), then on that machine run:

```sh
tar xzf disk-hog-linux-amd64.tar.gz
cd disk-hog
./deploy.sh
```

### Windows

Open PowerShell and run:

```powershell
git clone https://github.com/kishore-144/disk-hog.git
cd disk-hog
powershell -ExecutionPolicy Bypass -File deploy.ps1
```

This needs Go, or a `disk-hog.exe` from `make dist` placed next to
`deploy.ps1`. It installs to `%LOCALAPPDATA%\disk-hog` and adds a task that
starts it at logon. Add `-Uninstall` to remove it.

## Dashboard

| Tab | What it shows |
|---|---|
| Overview | Disk usage, scan controls with live progress, and space you can reclaim by category and risk. |
| Cleanup | Every finding. Filter by category, risk, age ("untouched 90+ days"), size or text. Select items, then **Clean** (safe items deleted, others trashed), **Move to Trash** or **Delete permanently**. You can also open an item in the file manager, copy its path, or ignore it. |
| Explore | ncdu-style drill-down through folder sizes, with findings highlighted. |
| Duplicates | Identical files, compared by size, then a hash of the first and last 16 KiB, then full SHA-256. Hard links don't count as duplicates. One copy of each group is always kept. |
| System | Suggestions with the exact command to run, sized for this machine: package-manager caches, journal, rotated logs, crash dumps, old kernels, snap revisions, Flatpak and Nix. Also Docker/Podman usage with prune buttons. |
| History | Every cleanup, plus total space freed. |
| Settings | Effective config, ignore list and the full rule list. |

## CLI

```sh
disk-hog scan                                  # what can be reclaimed in ~
disk-hog scan ~/code --older-than 60d          # stale projects only
disk-hog scan --category deps,build --json     # machine-readable
disk-hog clean --rule node_modules --older-than 90d          # dry run
disk-hog clean --category cache --yes                       # do it
disk-hog clean --risk review --match .iso --trash --yes     # to the trash
disk-hog dupes ~/Downloads ~/Pictures --min-size 5MB
disk-hog system        # sudo commands for package caches, journal, kernels...
disk-hog docker        # container storage
disk-hog disks         # filesystem usage
disk-hog rules         # every rule disk-hog knows
disk-hog config set schedule 24h
disk-hog token         # API token for remote access
```

`clean` is always a dry run unless you add `--yes`. By default it only
touches `safe` and `moderate` items. Add `--risk review` or `--risk all` to
include the rest.

## What it finds

Run `disk-hog rules` for the full list. In summary:

- **Dependencies**
  - `node_modules` (only next to a `package.json`)
  - Python venvs (only when they contain `pyvenv.cfg`)
  - Composer `vendor`, Go `vendor`, Mix `deps`, CocoaPods, Bower, Terraform providers
- **Build output.** Each is only flagged next to the project's own marker file:
  - Cargo, Maven, sbt and Gradle `target`/`build`, .NET `bin`/`obj`, CMake/Meson, Flutter, SwiftPM
  - Elixir/Erlang `_build`, Haskell, Zig, Unity, Unreal, Godot
  - Next/Nuxt/SvelteKit/Turbo/Parcel/Vite caches, `dist`, coverage reports
- **Caches**
  - npm, npx, Yarn, pnpm, Bun, Deno, pip, uv, Poetry, Conda
  - Go build and module caches, Cargo, Gradle, Maven, Ivy/sbt/Coursier, NuGet, Composer, pub, ccache, Bazel
  - Playwright, Puppeteer, Cypress, Hugging Face, PyTorch
  - Flatpak app caches, XDG `~/.cache/*`, macOS `~/Library/Caches/*`
- **Editors.** VS Code and its forks (Cursor, VSCodium, Windsurf): caches and workspace storage. JetBrains caches, Electron app caches, Xcode DerivedData.
- **Toolchains.** nvm/fnm Node versions, pyenv, rustup toolchains, SDKMAN candidates, Gradle wrappers, Conda environments, Xcode device support.
- **VMs and models.** VirtualBox, libvirt, Vagrant boxes, minikube, Lima/Colima, Android emulators and system images, Ollama and LM Studio models.
- **Trash, logs, temp.**
  - The trash
  - Core and heap dumps
  - Log files over 50 MB
  - Partial downloads over 10 MB
  - Windows `%TEMP%`
- **Large files.** Anything over 512 MB (configurable).

**Never touched:**
- Installed editor extensions and Python `site-packages`.
- Tools installed under hidden folders in your home, such as `~/.local/share/nvim/mason` or `~/.config/*/node_modules`.
- Any folder containing an empty `.disk-hog-keep` file.
- Any path on the ignore list.

**Risk levels:**
- `safe` — regenerated automatically, such as caches or `__pycache__`.
- `moderate` — needs a reinstall or rebuild, such as `node_modules` or venvs.
- `review` — not regenerable: large files, VMs, toolchains.

## Configuration

The config file is optional. `disk-hog config path` prints where it lives
on your system:
- Linux and BSD: `~/.config/disk-hog/config.json`
- macOS: `~/Library/Application Support/disk-hog/config.json`
- Windows: `%APPDATA%\disk-hog\config.json`

`disk-hog config init` writes the defaults there. See
[`config.example.json`](config.example.json) for all keys:

- `roots`, `exclude`
- `oneFileSystem`, `includeGlobal`
- `minItemSize`, `largeFileSize`
- `readOnly`
- `defaultAction` (`auto`, `trash` or `delete`)
- `schedule` (for example `24h`) and `notifyAbove` (for example `5GB`, sends a desktop notification)
- `disabledRules`
- `rules`, for your own rules, in the same format as the built-in ones

Restart after editing: run `./deploy.sh restart` from the cloned folder.

## Security

- **Network.** It binds to `127.0.0.1` by default. It only answers requests
  whose Host is a loopback name, which blocks DNS rebinding.
- **Changes.** Every request that changes anything needs a random token
  for that server process plus a same-origin check. Only a page loaded from
  `localhost` receives the token automatically.
- **Remote access.** If you set `"host": "0.0.0.0"` to use it from another
  device, every API call needs the token from `disk-hog token`. The
  dashboard asks for it.
- **Deletion checks.** Before anything is removed, the path is checked
  again. Removal is refused if any of these is true:
  - it is not canonical;
  - it is outside the scanned roots and known cache paths;
  - it is a protected folder (`/`, home, `~/Documents`, `~/.ssh`, `/usr`, …)
    or contains one;
  - it became a symlink;
  - its device or inode number changed since the scan.
- **Read-only mode.** `"readOnly": true` (or `serve --read-only`) turns all
  deletion off.

## Develop

```sh
git clone https://github.com/kishore-144/disk-hog.git
cd disk-hog
make build      # build/disk-hog
make run        # serve with debug logs
make test       # unit tests (make race for the race detector)
make dist       # release archives for 16 platforms + SHA256SUMS
```

The layout:
- `cmd/disk-hog` — CLI
- `internal/scan` — parallel walker and size tree
- `internal/rules` — rule catalogue
- `internal/clean` — guarded removal
- `internal/fsutil` — per-OS pieces: trash, disks, priority
- `internal/dupes`, `internal/advisor`, `internal/docker`
- `internal/server` — HTTP, SSE and the embedded `web/`
