# Archivist Sync

Archivist Sync keeps folders on your computer in [The Archivist](https://the-archivist.cc), your private, searchable lifelong archive. It's one way only: it reads the folders you choose and sends their changes to your archive.

- **Install it once,** with one line, or ask your AI assistant to.
- **Choose folders on a page in your browser.**
- **Then forget it.** It starts by itself when you log in, keeps itself up to date, and asks only when something needs you.

It's free and open source (MIT). You need an account with The Archivist.

## Installing

**macOS:** open Terminal (press ⌘ Space, type Terminal, press Return), paste this line and press Return:

```sh
curl -fsSL https://the-archivist.cc/sync/install.sh | sh
```

**Windows:** open PowerShell (press the Windows key, type PowerShell, press Enter), paste this line and press Enter:

```powershell
irm https://the-archivist.cc/sync/install.ps1 | iex
```

**Linux:** the same line as macOS, in a terminal.

No administrator password is needed. The page opens in your browser:
1. Press **Connect** and approve this computer in your archive.
2. Choose your folders.

### With an AI assistant

Ask an assistant that can run commands on your computer:

> Set up Archivist Sync on this computer so my Documents folder syncs to my archive. Follow https://the-archivist.cc/sync/agents.md.

## What it promises

- **One way, read-only.** It never changes, moves or deletes a file on your computer.
- **Only what you chose.** It reads only the folders you add, and only files your archive takes. Nothing else leaves the computer.
- **Limited access.** Its sign-in can add, update and remove documents in the folders it syncs. It can't read or search your archive.
- **No telemetry.** It contacts only your archive and, once a day, GitHub, to look for updates.
- **No accidental emptying.** If a sync would remove many documents (an unplugged drive, a folder moved away), it waits for you to say so.
- **Only signed updates.** It installs an update only if it's signed with the project's release key.

## What it syncs

Files your archive takes, up to 10 MB each:
- Markdown and plain text;
- PDF, Word (`.docx`), OpenDocument text, spreadsheets (`.xlsx`, `.xls`, `.ods`, `.numbers`, `.csv`), and web pages and XML.

Your archive turns these into text and keeps only the text.

Hidden files and folders, and system clutter such as `.DS_Store` and Office lock files, are never sent. Anything else is listed on the page as left out.

It checks your folders for changes every minute. It syncs about two minutes after you stop changing things, and at least every fifteen minutes.

## Commands

The page does everything, but every step is also a command. That's how an AI assistant sets it up:

```
archivist-sync open                 open the settings page
archivist-sync connect              connect this computer (prints a link to approve)
archivist-sync add <folder>         start syncing a folder
    --name <name>                   its folder name in your archive
    --into <name>                   continue a folder already in your archive
archivist-sync remove <folder>      stop syncing; what's in your archive stays
archivist-sync status               what's syncing
archivist-sync sync [<folder>]      sync now
archivist-sync update               install the latest version now
archivist-sync uninstall            remove it from this computer
```

Add `--json` for structured output. Commands exit 0 on success, and otherwise print one line saying what's wrong.

## Where things are

| | Program | Settings |
| --- | --- | --- |
| macOS | `~/Library/Application Support/Archivist Sync/archivist-sync` | same folder |
| Windows | `%LOCALAPPDATA%\Archivist Sync\archivist-sync.exe` | `%APPDATA%\Archivist Sync` |
| Linux | `~/.local/share/archivist-sync/archivist-sync`, linked from `~/.local/bin` | `~/.config/archivist-sync` |

The settings file holds your sign-in and is readable only by you. The page is served at `http://127.0.0.1:47811/` on your computer alone, and needs a key from the settings file.

## Developing

You need Go 1.24 or later.

```sh
go test ./...
go run . run       # the background program, with its page
```

Set `ARCHIVIST_URL` to sync to another archive, such as a local one at `http://localhost:8787`, and `ARCHIVIST_SYNC_HOME` to keep its settings somewhere else.

Releases come from a `vX.Y.Z` tag. The release workflow builds the program for every system, writes `SHA256SUMS` and signs it with the project's minisign key, which is a repository secret.
