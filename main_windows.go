//go:build windows

package main

import (
	_ "embed"
	"math"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed ui.html
var uiHTML []byte

const appName = "EDTracker Pro Tool"

var (
	modShell32        = syscall.NewLazyDLL("shell32.dll")
	procIsUserAnAdmin = modShell32.NewProc("IsUserAnAdmin")
	procShellExecuteW = modShell32.NewProc("ShellExecuteW")
	procMessageBoxW   = modUser32.NewProc("MessageBoxW")
	procCreateMutexW  = modKernel32.NewProc("CreateMutexW")
)

// ---------- settings ----------

type Settings struct {
	Recentre    Binding `json:"recentre"`
	Toggle      Binding `json:"toggle"`
	Sound       bool    `json:"sound"`
	AlwaysAdmin bool    `json:"alwaysAdmin"`
	FT          bool    `json:"freetrack"`
	FTNoDummy   bool    `json:"freetrackNoDummy"`
	FTInv       [3]bool `json:"freetrackInvert"`
	UDP         bool    `json:"udp"`
	UDPHost     string  `json:"udpHost"`
	UDPPort     int     `json:"udpPort"`
	Mode        string  `json:"mode"` // "tracker" (default) or "receiver"
	RecvPort    int     `json:"recvPort"`
	VJoy        bool    `json:"vjoy"`
	VJoyID      int     `json:"vjoyId"`
	VJoyAxes    string  `json:"vjoyAxes"`
	RollOff     bool    `json:"rollOff"`
}

func receiverMode() bool { settingsMu.Lock(); defer settingsMu.Unlock(); return settings.Mode == "receiver" }

// publish sends a pose (degrees, before inversion) to every enabled output.
func publish(deg [3]float64, fromTracker bool) {
	settingsMu.Lock()
	inv, rollOff := settings.FTInv, settings.RollOff
	settingsMu.Unlock()
	if rollOff {
		deg[2] = 0
	}
	st.mu.Lock()
	on := st.outputOn
	st.mu.Unlock()
	if !fromTracker && !on { // Toggle output in receiver mode: hold the view centred
		deg = [3]float64{}
	}
	for i := range deg {
		if inv[i] {
			deg[i] = -deg[i]
		}
	}
	ft.Pose(deg[0], deg[1], deg[2])
	vjoy.Pose(deg[0], deg[1], deg[2])
	if fromTracker {
		udpOut.Pose(deg[0], deg[1], deg[2])
	}
}

// onReceived handles a pose arriving over the network in Receiver mode.
func onReceived(yaw, pitch, roll float64) {
	if !receiverMode() {
		return
	}
	toJoy := func(d float64) int { return int(math.Max(-32768, math.Min(32767, d*32768/180))) }
	st.mu.Lock()
	settingsMu.Lock()
	if settings.RollOff {
		roll = 0
	}
	settingsMu.Unlock()
	st.joy, st.joyAt = [3]int{toJoy(yaw), toJoy(pitch), toJoy(roll)}, time.Now()
	st.mu.Unlock()
	publish([3]float64{yaw, pitch, roll}, false)
}

func recentre() error {
	if receiverMode() {
		recv.Recentre()
		return nil
	}
	return sendCommand(1)
}

var (
	settingsMu sync.Mutex
	settings   = Settings{Sound: true}
)

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "EDTrackerProTool", "settings.json")
}

func loadSettings() {
	b, err := os.ReadFile(settingsPath())
	if err == nil {
		_ = json.Unmarshal(b, &settings)
	}
}

func saveSettings() {
	p := settingsPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(settings, "", "  ")
	_ = os.WriteFile(p, b, 0o644)
}

// ---------- helpers ----------

func isAdmin() bool { r, _, _ := procIsUserAnAdmin.Call(); return r != 0 }

func msgBox(text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(appName)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x40)
}

func relaunchAsAdmin() bool {
	exe, _ := os.Executable()
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	args, _ := syscall.UTF16PtrFromString("--elevated")
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(args)), 0, 1)
	return r > 32
}

// ---------- device state ----------

type state struct {
	mu        sync.Mutex
	dev       *hidDevice
	aux       []byte
	auxAt     time.Time
	joy       [3]int
	joyAt     time.Time
	joyFall   int // winmm id when HidD_GetInputReport isn't supported, -1 none
	outputOn  bool
	capturing string
	capSnap   captureSnapshot
	capResult *Binding
	capCancel bool
	capUntil  time.Time
	events    []map[string]any
}

var st = &state{joyFall: -1, outputOn: true}

func (s *state) event(kind, msg string) {
	s.mu.Lock()
	s.events = append(s.events, map[string]any{"t": time.Now().Format("15:04:05"), "kind": kind, "msg": msg})
	if len(s.events) > 50 {
		s.events = s.events[len(s.events)-50:]
	}
	s.mu.Unlock()
}

func (s *state) device() *hidDevice { s.mu.Lock(); defer s.mu.Unlock(); return s.dev }

func pollLoop() {
	fails := 0
	joyFails := 0
	for {
		if receiverMode() {
			if d := st.device(); d != nil {
				d.Close()
				st.mu.Lock()
				st.dev, st.aux = nil, nil
				st.mu.Unlock()
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		d := st.device()
		if d == nil {
			nd, err := findTracker()
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}
			st.mu.Lock()
			st.dev, st.joyFall = nd, -1
			st.mu.Unlock()
			fails, joyFails = 0, 0
			st.event("info", fmt.Sprintf("Connected: %s (%04X:%04X), feature report %d bytes", nd.Product, nd.VendorID, nd.ProductID, nd.FeatureLen))
			continue
		}
		if b, err := d.GetFeature(5); err == nil {
			st.mu.Lock()
			st.aux, st.auxAt = b, time.Now()
			st.mu.Unlock()
			fails = 0
		} else if fails++; fails > 40 {
			st.event("warn", "Lost contact with tracker: "+err.Error())
			d.Close()
			st.mu.Lock()
			st.dev, st.aux = nil, nil
			st.mu.Unlock()
			continue
		}
		// live angles: direct input report first, winmm joystick as fallback
		var ang [3]int
		got := false
		if joyFails < 20 {
			if j, err := d.GetJoystick(); err == nil {
				ang, got = [3]int{int(j[0]), int(j[1]), int(j[2])}, true
				joyFails = 0
			} else {
				joyFails++
			}
		}
		if !got {
			st.mu.Lock()
			if st.joyFall < 0 {
				st.joyFall = trackerJoystick(d.VendorID, d.ProductID)
			}
			jf := st.joyFall
			st.mu.Unlock()
			if jf >= 0 {
				if ji, ok := joyState(jf); ok {
					ang, got = [3]int{int(ji.X) - 32768, int(ji.Y) - 32768, int(ji.Z) - 32768}, true
				}
			}
		}
		if got {
			st.mu.Lock()
			status := -1
			if len(st.aux) > 24 {
				status = int(st.aux[24]) // AUX deviceStatus (byte 23 of the payload, after the report ID)
			}
			st.mu.Unlock()
			deg := [3]float64{}
			for i := 0; i < 3; i++ {
				deg[i] = float64(ang[i]) * 180 / 32768
			}
			out, held := trackerHold.Filter(deg, status, time.Now())
			st.mu.Lock()
			if held {
				st.joy = [3]int{} // the head view shows what the games get
			} else {
				st.joy = ang
				settingsMu.Lock()
				if settings.RollOff {
					st.joy[2] = 0
				}
				settingsMu.Unlock()
			}
			st.joyAt = time.Now()
			st.mu.Unlock()
			publish(out, true)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func sendCommand(cmd byte) error {
	d := st.device()
	if d == nil {
		return fmt.Errorf("tracker not connected")
	}
	noteRecentreCommand(cmd)
	return d.SetFeature(4, []byte{cmd, 0})
}

var lastRecentre time.Time

// noteRecentreCommand starts the output hold for commands that make the firmware recentre.
func noteRecentreCommand(cmd byte) {
	if cmd == 1 || cmd == 2 || cmd == 9 { // recentre, gyro bias, full bias
		trackerHold.Start()
		st.mu.Lock()
		lastRecentre = time.Now()
		st.mu.Unlock()
	}
}

func hotkeyLoop() {
	var lastRe, lastTog bool
	for {
		time.Sleep(15 * time.Millisecond)
		st.mu.Lock()
		capturing := st.capturing
		st.mu.Unlock()
		if capturing != "" {
			b, ok := pollCapture(&st.capSnap)
			st.mu.Lock()
			if ok {
				st.capResult, st.capCancel = &b, b.Type == ""
				st.capturing = ""
			} else if time.Now().After(st.capUntil) {
				st.capResult, st.capCancel = &Binding{}, true
				st.capturing = ""
			}
			st.mu.Unlock()
			lastRe, lastTog = true, true // don't fire on the key that was just captured
			continue
		}
		settingsMu.Lock()
		re, tog, snd := settings.Recentre, settings.Toggle, settings.Sound
		settingsMu.Unlock()
		pRe, pTog := re.pressed(), tog.pressed()
		if pRe && !lastRe {
			if err := recentre(); err == nil {
				st.event("hotkey", "Recentred ("+re.describe()+")")
				if snd {
					beep()
				}
			} else {
				st.event("warn", "Recentre hotkey: "+err.Error())
			}
		}
		if pTog && !lastTog {
			st.mu.Lock()
			st.outputOn = !st.outputOn
			on := st.outputOn
			st.mu.Unlock()
			cmd := byte(14)
			if on {
				cmd = 15
			}
			var err error
			if !receiverMode() {
				err = sendCommand(cmd)
			}
			if err == nil {
				st.event("hotkey", fmt.Sprintf("Output %s (%s)", map[bool]string{true: "on", false: "off"}[on], tog.describe()))
				if snd {
					beep()
				}
			}
		}
		lastRe, lastTog = pRe, pTog
	}
}

// ---------- bindings exposed to the UI ----------

func bindAll(w webview2.WebView) {
	w.Bind("edtStatus", func() map[string]any {
		st.mu.Lock()
		d := st.dev
		ev := st.events
		st.events = nil
		out := st.outputOn
		st.mu.Unlock()
		settingsMu.Lock()
		s := settings
		settingsMu.Unlock()
		m := map[string]any{"connected": d != nil, "admin": isAdmin(), "events": ev, "outputOn": out,
			"sound": s.Sound, "alwaysAdmin": s.AlwaysAdmin,
			"recentre": s.Recentre.describe(), "toggle": s.Toggle.describe(),
			"reDown": s.Recentre.Type != "" && s.Recentre.pressed(), "togDown": s.Toggle.Type != "" && s.Toggle.pressed()}
		fe, fg, ferr, ff := ft.Status()
		m["ft"], m["ftGame"], m["ftError"], m["ftFrames"], m["ftInv"], m["ftDummy"] = fe, fg, ferr, ff, s.FTInv, !s.FTNoDummy
		for _, part := range []map[string]any{udpOut.Status(), recv.Status(), vjoy.Status()} {
			for k, v := range part {
				m[k] = v
			}
		}
		m["mode"] = map[bool]string{true: "receiver", false: "tracker"}[s.Mode == "receiver"]
		m["vjoyWanted"], m["vjoyId"], m["vjoyAxes"] = s.VJoy, s.VJoyID, s.VJoyAxes
		m["rollOff"] = s.RollOff
		if d != nil {
			m["product"], m["vid"], m["pid"], m["featureLen"] = d.Product, d.VendorID, d.ProductID, d.FeatureLen
		}
		return m
	})
	w.Bind("edtGetFeature", func(id int) ([]int, error) {
		d := st.device()
		if d == nil {
			return nil, fmt.Errorf("tracker not connected")
		}
		var b []byte
		if id == 5 {
			st.mu.Lock()
			if time.Since(st.auxAt) < 150*time.Millisecond {
				b = append([]byte(nil), st.aux...)
			}
			st.mu.Unlock()
		}
		if b == nil {
			var err error
			if b, err = d.GetFeature(byte(id)); err != nil {
				return nil, err
			}
		}
		out := make([]int, len(b))
		for i, v := range b {
			out[i] = int(v)
		}
		return out, nil
	})
	w.Bind("edtSetFeature", func(id int, data []int) error {
		d := st.device()
		if d == nil {
			return fmt.Errorf("tracker not connected")
		}
		p := make([]byte, len(data))
		for i, v := range data {
			p[i] = byte(v)
		}
		if id == 4 && len(p) > 0 {
			noteRecentreCommand(p[0])
		}
		if id == 4 && len(p) > 0 && (p[0] == 14 || p[0] == 15) {
			st.mu.Lock()
			st.outputOn = p[0] == 15
			st.mu.Unlock()
		}
		return d.SetFeature(byte(id), p)
	})
	w.Bind("edtGetJoy", func() []int {
		st.mu.Lock()
		defer st.mu.Unlock()
		if time.Since(st.joyAt) > 500*time.Millisecond {
			return nil
		}
		return []int{st.joy[0], st.joy[1], st.joy[2]}
	})
	w.Bind("edtStartCapture", func(which string) {
		st.mu.Lock()
		st.capSnap = takeSnapshot()
		st.capturing, st.capResult, st.capCancel = which, nil, false
		st.capUntil = time.Now().Add(10 * time.Second)
		st.mu.Unlock()
	})
	w.Bind("edtCaptureResult", func() map[string]any {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.capturing != "" || st.capResult == nil {
			return map[string]any{"done": false}
		}
		return map[string]any{"done": true, "cancelled": st.capCancel, "label": st.capResult.describe(), "binding": st.capResult}
	})
	w.Bind("edtSetBinding", func(which string, b Binding) string {
		settingsMu.Lock()
		if which == "recentre" {
			settings.Recentre = b
		} else {
			settings.Toggle = b
		}
		saveSettings()
		settingsMu.Unlock()
		return b.describe()
	})
	w.Bind("edtSetOption", func(name string, v bool) {
		settingsMu.Lock()
		switch name {
		case "sound":
			settings.Sound = v
		case "alwaysAdmin":
			settings.AlwaysAdmin = v
		case "ft":
			settings.FT = v
		case "ftDummy":
			settings.FTNoDummy = !v
		case "rollOff":
			settings.RollOff = v
		case "ftInv0", "ftInv1", "ftInv2":
			settings.FTInv[name[5]-'0'] = v
		}
		saveSettings()
		dummy := !settings.FTNoDummy
		settingsMu.Unlock()
		switch name {
		case "ft":
			if v {
				if err := ft.Enable(dummy); err != nil {
					st.event("warn", "FreeTrack output: "+err.Error())
				} else {
					st.event("info", "FreeTrack/TrackIR output on")
				}
			} else {
				ft.Disable()
				st.event("info", "FreeTrack/TrackIR output off")
			}
		case "ftDummy":
			if en, _, _, _ := ft.Status(); en { // restart to apply
				ft.Disable()
				ft.Enable(dummy)
			}
		}
	})
	w.Bind("edtSetUDP", func(enabled bool, host string, port int) map[string]any {
		settingsMu.Lock()
		settings.UDP, settings.UDPHost, settings.UDPPort = enabled, host, port
		saveSettings()
		settingsMu.Unlock()
		udpOut.Configure(enabled, host, port)
		if enabled {
			st.event("info", fmt.Sprintf("UDP output on → %s:%d", host, port))
		} else {
			st.event("info", "UDP output off")
		}
		return udpOut.Status()
	})
	w.Bind("edtRecentre", func() error { return recentre() })
	w.Bind("edtSetMode", func(mode string) {
		settingsMu.Lock()
		if mode != "receiver" {
			mode = "tracker"
		}
		settings.Mode = mode
		port := settings.RecvPort
		saveSettings()
		settingsMu.Unlock()
		if mode == "receiver" {
			if err := recv.Start(port); err != nil {
				st.event("warn", err.Error())
			} else {
				st.event("info", fmt.Sprintf("Receiver mode: listening on UDP %d", port))
			}
		} else {
			recv.Stop()
			st.event("info", "Tracker mode")
		}
	})
	w.Bind("edtSetReceiver", func(port int) {
		if port <= 0 || port > 65535 {
			port = 4242
		}
		settingsMu.Lock()
		settings.RecvPort = port
		saveSettings()
		settingsMu.Unlock()
		if receiverMode() {
			if err := recv.Start(port); err != nil {
				st.event("warn", err.Error())
			} else {
				st.event("info", fmt.Sprintf("Listening on UDP %d", port))
			}
		}
	})
	w.Bind("edtSetVJoy", func(enabled bool, id int, axes string) {
		settingsMu.Lock()
		settings.VJoy, settings.VJoyID, settings.VJoyAxes = enabled, id, axes
		saveSettings()
		settingsMu.Unlock()
		if enabled {
			if err := vjoy.Enable(id, axes); err != nil {
				st.event("warn", "vJoy: "+err.Error())
			} else {
				st.event("info", fmt.Sprintf("vJoy device %d active", id))
			}
		} else {
			vjoy.Disable()
		}
	})
	w.Bind("edtRestartAdmin", func() bool {
		if relaunchAsAdmin() {
			go func() { ft.Disable(); time.Sleep(300 * time.Millisecond); os.Exit(0) }()
			return true
		}
		return false
	})
}

func main() {
	loadSettings()
	elevatedFlag := len(os.Args) > 1 && os.Args[1] == "--elevated"
	if settings.AlwaysAdmin && !isAdmin() && !elevatedFlag {
		if relaunchAsAdmin() {
			return
		}
	}
	name, _ := syscall.UTF16PtrFromString("EDTrackerProTool.SingleInstance")
	var err error
	for i := 0; i < 30; i++ { // an elevated relaunch may start before the old instance has exited
		_, _, err = procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
		if err != syscall.Errno(183) || !elevatedFlag {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err == syscall.Errno(183) { // ERROR_ALREADY_EXISTS
		msgBox("EDTracker Pro Tool is already running.")
		return
	}

	// serve the UI locally
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		msgBox("Could not start the UI server: " + lerr.Error())
		return
	}
	go http.Serve(ln, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		rw.Write(uiHTML)
	}))

	startKeyboardHook()
	defer ft.Disable()
	if settings.UDPPort == 0 {
		settings.UDPPort = 4242
	}
	udpOut.Configure(settings.UDP, settings.UDPHost, settings.UDPPort)
	if settings.RecvPort == 0 {
		settings.RecvPort = 4242
	}
	if settings.VJoyID == 0 {
		settings.VJoyID = 1
	}
	if settings.VJoyAxes == "" {
		settings.VJoyAxes = "xyz"
	}
	recv.onPose = onReceived
	recv.onEvent = st.event
	onUDPEvent = st.event
	onRemoteRecentre = func() error {
		if receiverMode() {
			return fmt.Errorf("this PC is in Receiver mode")
		}
		st.mu.Lock()
		recent := time.Since(lastRecentre) < time.Second
		st.mu.Unlock()
		if recent { // same key bound on both PCs: we've just recentred, don't restart the firmware's settle
			return nil
		}
		if err := sendCommand(1); err != nil {
			return err
		}
		settingsMu.Lock()
		snd := settings.Sound
		settingsMu.Unlock()
		if snd {
			beep()
		}
		return nil
	}
	defer recv.Stop()
	if settings.Mode == "receiver" {
		if err := recv.Start(settings.RecvPort); err != nil {
			st.event("warn", err.Error())
		} else {
			st.event("info", fmt.Sprintf("Receiver mode: listening on UDP %d", settings.RecvPort))
		}
	}
	defer vjoy.Disable()
	if settings.VJoy {
		if err := vjoy.Enable(settings.VJoyID, settings.VJoyAxes); err != nil {
			st.event("warn", "vJoy: "+err.Error())
		} else {
			st.event("info", fmt.Sprintf("vJoy device %d active", settings.VJoyID))
		}
	}
	if settings.FT {
		if err := ft.Enable(!settings.FTNoDummy); err != nil {
			st.event("warn", "FreeTrack output: "+err.Error())
		} else {
			st.event("info", "FreeTrack/TrackIR output on")
		}
	}
	hookMu.Lock()
	hk := hookActive
	hookMu.Unlock()
	st.event("info", fmt.Sprintf("Hotkeys ready — keyboard hook %s, %s", map[bool]string{true: "active", false: "unavailable"}[hk], map[bool]string{true: "running as administrator", false: "NOT running as administrator"}[isAdmin()]))
	go pollLoop()
	go hotkeyLoop()

	local, _ := os.UserCacheDir()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(local, "EDTrackerProTool", "WebView2"),
		WindowOptions: webview2.WindowOptions{
			Title: appName, Width: 1240, Height: 940, IconId: 2, Center: true,
		},
	})
	if w == nil {
		msgBox("This program needs the Microsoft Edge WebView2 Runtime, which wasn't found.\n\nInstall it from https://developer.microsoft.com/microsoft-edge/webview2/ (Evergreen Runtime) and try again.\n\nHotkeys are not active.")
		return
	}
	defer w.Destroy()
	bindAll(w)
	w.Init("window.__EDT_NATIVE = true;")
	w.Navigate(fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port))
	w.Run()
}
