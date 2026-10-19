# ZDownloader

![License MIT](https://img.shields.io/badge/license-MIT-blue)
![Version](https://img.shields.io/badge/version-1.7.2-blue)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey)

English | [简体中文](README.zh-CN.md)

A free and open-source multi-protocol downloader. One single program file, ready to use out of the box — HTTP multi-thread downloads, BT/magnet, m3u8 video streams and RSS subscriptions are all built in, with a native desktop UI and a browser extension that takes over your downloads.

## Features

**HTTP downloads**
- Multi-threaded segmented download, up to 64 connections, making the most of your bandwidth
- Resume interrupted downloads, even after a reboot
- Global and per-task speed limits
- HTTP / SOCKS5 proxy support, configurable per usage (downloads, trackers, peers)
- Automatic file name and size detection; existing files are never overwritten

**Video & streaming**
- Paste an m3u8 link and download — best quality picked automatically, stream decrypted and merged into a single file

**BitTorrent**
- Magnet links and .torrent files (v1 / v2)
- Select individual files from the file list and download only what you need
- Auto-seeding after completion, stopped automatically by share ratio or time limit
- Built-in protection that detects and bans leeching or fake peers

**Everyday use**
- Browser extension: takes over browser downloads, sniffs videos and audio on the page, download via right-click
- RSS subscriptions: automatically downloads newly released episodes
- System tray, floating speed window, launch at login, dark / light theme
- System notifications on download completion
- Double-click a magnet link or .torrent file — it is handed to the running instance instead of opening a second one

## Supported Platforms

| Platform | Status |
|---|---|
| Windows 10/11 | ✅ Full support |
| macOS 11+ (Intel / Apple Silicon) | ✅ Full support |
| Linux | ⚠️ Works, desktop integration (tray etc.) not yet available |

## Installation

Download the archive for your platform from the [Releases](../../releases) page, extract it, and run:

- **Windows**: double-click `zdownloader.exe`
- **macOS**: open `ZDownloader.app`
- **Linux**: run `zdownloader`, then open the UI in your browser

Add the `-headless` flag to run without any window (HTTP service only, ideal for NAS / servers).

### Browser extension (optional)

Open `chrome://extensions` in Chrome → enable "Developer mode" → "Load unpacked" → select the `extension/` directory from the archive.

### Command line options

| Option | Description |
|---|---|
| `-port N` | Listen port (default 8620, auto-increments if busy) |
| `-dir PATH` | Download directory |
| `-data PATH` | Data directory |
| `-no-tray` | Do not show the tray icon |
| `-headless` | Run without a window, HTTP service only |

## Build from Source

Requires Go 1.25+ and Node.js 18+ (macOS additionally needs Xcode Command Line Tools):

```bash
# macOS
./scripts/build.sh

# Windows
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```

Build artifacts go to `dist/`, including the executable, the browser extension and the macOS `.app` bundle.

## Privacy

- All download history and settings stay on your computer — nothing is uploaded anywhere
- The UI service listens on localhost (`127.0.0.1`) only and is not reachable from other devices
- Browser extension settings are stored locally by Chrome

## Roadmap

- [ ] DASH (.mpd) support
- [ ] Remux TS to MP4
- [ ] Download queue priority / scheduled downloads
- [ ] Linux desktop integration
- [ ] Multi-language UI

## License

[MIT](LICENSE) © mbzmhs
