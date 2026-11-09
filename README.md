# EDTracker Pro Tool

A replacement for the EDTracker Pro configuration software, written from scratch.

- **Calibration**: wizard (bias, mounting orientation, magnetometer cloud with ellipsoid fit), settings (orientation, response mode, yaw/pitch/roll scale 1–10× or roll off, smoothing), backup/restore of the stored calibration.
- **Live 3D head**, always visible, with per-axis invert for the view (pitch inverted by default).
- **Global hotkeys** (keyboard combination or any joystick/HOTAS button) to recentre or toggle output, working while Elite Dangerous has focus.
- **Outputs**: FreeTrack / TrackIR for games on the same PC, and OpenTrack-compatible **UDP over network**.
- **Receiver mode**: run the same exe on another PC (e.g. a Shadow PC over Tailscale) to turn the UDP stream into FreeTrack/TrackIR or a **vJoy** virtual joystick, with no OpenTrack needed at either end.
- **Clean recentre**: the firmware's smoothing sweep after a recentre is hidden, and recentring on the Receiver recentres the tracker itself on the sending PC.

## Download

Every push builds the exe automatically: see the **Actions** tab → latest run → *Artifacts*.
Released versions (`v1.0.0`, `v1.1.0`, …) appear under **Releases** as a ready-to-run zip.

## Running

1. Put `EDTrackerProTool.exe` anywhere and run it. Windows SmartScreen may warn because the exe isn't signed: *More info → Run anyway*.
2. It needs the Microsoft Edge **WebView2 Runtime**, which Windows 10/11 normally already has (you'll get a download link if not).
3. Close the old EDTracker Pro UI first.

Settings are saved in `%APPDATA%\EDTrackerProTool\settings.json`.

### Hotkeys
Hotkeys tab → *Assign…* → press a key combination or a joystick button (Esc cancels). If Elite (or its launcher) runs as administrator, this tool must too: use *Restart as administrator* and tick *Always start as administrator*. Hotkeys work while the window is minimised.

### FreeTrack / TrackIR (same PC)
Output tab → tick *FreeTrack / TrackIR*. The tracker still appears as a joystick; the tool also publishes the angles the way OpenTrack's *freetrack 2.0 Enhanced* output does. In Elite, remove the joystick axes bound to head look so it isn't read twice. Don't run OpenTrack's FreeTrack output at the same time.

### UDP over network (e.g. to a Shadow PC)
Output tab → tick *UDP over network*, enter the other machine's Tailscale address or MagicDNS name (e.g. `shadow-pc`), port 4242, *Apply*. The data is identical to OpenTrack's UDP output, so OpenTrack (input *UDP over network*) or this tool in Receiver mode can receive it.

### Receiver mode
On the receiving PC click **Receiver** (top right). It listens on UDP 4242 and outputs FreeTrack/TrackIR and/or vJoy (Output tab). Allow it through the Windows firewall when asked. Use vJoy *or* FreeTrack for Elite, not both. vJoy needs `vJoyInterface.dll` (installed with vJoy in `Program Files\vJoy\x64`, or copy it next to the exe).

Recentre on the Receiver (button or hotkey) sends a small request back along the same UDP path; the sending PC recentres the tracker and acknowledges. If the sender is OpenTrack, the Receiver falls back to a local recentre. It's fine to bind the same hotkey on both PCs: a duplicate request within a second is ignored.

### Browser version
`ui.html` opened in Chrome or Edge works on its own (via WebHID) for calibration and settings, without hotkeys, outputs or Receiver mode.

## Building

Built in Go; the build is cross-compiled from any OS. The GitHub Actions workflow (`.github/workflows/build.yml`) does exactly this:

```sh
export GOPROXY=direct GOFLAGS=-mod=mod   # go.mod uses the GitHub mirror of golang.org/x/sys
go test ./...                            # receiver, UDP, recentre and hold logic
go install github.com/akavel/rsrc@v0.10.2
rsrc -manifest app.manifest -ico app.ico -arch amd64 -o rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-H windowsgui -s -w" -o EDTrackerProTool.exe .
```

To publish a release: on GitHub, **Releases → Draft a new release**, type a new tag such as `v1.1.0`, and **Publish**. The workflow builds the exe and attaches it to the release a minute later. (Pushing a `v*` tag from git does the same.)

## Code map

| File | What it does |
|---|---|
| `main_windows.go` | Program start, settings, polling the tracker, hotkey loop, bindings to the UI |
| `hid_windows.go` | Talks to the tracker over Windows HID feature reports |
| `input_windows.go`, `hook_windows.go` | Hotkeys: low-level keyboard hook, joystick buttons, key capture |
| `ft_windows.go` | FreeTrack/TrackIR shared memory, registry and game key table |
| `vjoy_windows.go` | vJoy virtual joystick output |
| `udp.go` | OpenTrack-format UDP sender, and answering recentre requests |
| `receiver.go` | Receiver mode: UDP listener, recentre request/acknowledge, view hold |
| `hold.go` | Hides the firmware's smoothing sweep after a recentre |
| `ui.html` | The whole interface (embedded in the exe; also works standalone in a browser) |
| `ftfiles/` | OpenTrack's FreeTrack/TrackIR client DLLs, placeholder TrackIR.exe and game list |
| `docs/PROTOCOL.md` | The EDTracker Pro USB protocol, as recovered from the original software |

## Credits

The FreeTrack/TrackIR client DLLs, `TrackIR.exe` placeholder and supported-games list in `ftfiles/` come from [OpenTrack](https://github.com/opentrack/opentrack) under the ISC licence. See `ftfiles/OPENTRACK-NOTICE.txt`.
