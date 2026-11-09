//go:build windows

package main

// FreeTrack / TrackIR output, compatible with OpenTrack's "freetrack 2.0 enhanced" protocol.
// Games load NPClient(64).dll / freetrackclient(64).dll (OpenTrack builds, ISC licence) from the
// folder named in the registry; those DLLs read the "FT_SharedMem" block we fill here.

import (
	"embed"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

//go:embed ftfiles/*
var ftFiles embed.FS

var (
	modAdvapi32           = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW   = modAdvapi32.NewProc("RegCreateKeyExW")
	procRegSetValueExW    = modAdvapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW  = modAdvapi32.NewProc("RegQueryValueExW")
	procRegCloseKey       = modAdvapi32.NewProc("RegCloseKey")
	procCreateFileMapping = modKernel32.NewProc("CreateFileMappingW")
	procMapViewOfFile     = modKernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile   = modKernel32.NewProc("UnmapViewOfFile")
)

const (
	hkeyCurrentUser = 0x80000001
	keyAllAccess    = 0xF003F
	regSZ           = 1
	pageReadWrite   = 0x04
	fileMapAll      = 0xF001F
	ftHeapSize      = 108 // FTData (23×4) + GameID + table[8] + GameID2

	// byte offsets inside FTHeap
	offDataID   = 0
	offCamW     = 4
	offCamH     = 8
	offYaw      = 12
	offPitch    = 16
	offRoll     = 20
	offX        = 24
	offRawYaw   = 36
	offRawPitch = 40
	offRawRoll  = 44
	offGameID   = 92
	offTable    = 96
	offGameID2  = 104
)

type ftOutput struct {
	mu       sync.Mutex
	enabled  bool
	dir      string
	mapping  uintptr
	mutex    uintptr
	base     uintptr
	lastGame int32
	game     string
	err      string
	dummy    *exec.Cmd
	frames   uint64
}

var ft = &ftOutput{}

func (f *ftOutput) u32(off uintptr) *uint32 { return (*uint32)(unsafe.Pointer(f.base + off)) }

func (f *ftOutput) storeF(off uintptr, v float32) { atomic.StoreUint32(f.u32(off), math.Float32bits(v)) }

func regSetString(sub, name, value string) error {
	var key uintptr
	s, _ := syscall.UTF16PtrFromString(sub)
	r, _, _ := procRegCreateKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(s)), 0, 0, 0, keyAllAccess, 0, uintptr(unsafe.Pointer(&key)), 0)
	if r != 0 {
		return fmt.Errorf("registry %s: error %d", sub, r)
	}
	defer procRegCloseKey.Call(key)
	n, _ := syscall.UTF16PtrFromString(name)
	v, _ := syscall.UTF16FromString(value)
	r, _, _ = procRegSetValueExW.Call(key, uintptr(unsafe.Pointer(n)), 0, regSZ, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2))
	if r != 0 {
		return fmt.Errorf("registry %s\\%s: error %d", sub, name, r)
	}
	return nil
}

func regGetString(sub, name string) string {
	var key uintptr
	s, _ := syscall.UTF16PtrFromString(sub)
	if r, _, _ := procRegCreateKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(s)), 0, 0, 0, 0x20019, 0, uintptr(unsafe.Pointer(&key)), 0); r != 0 {
		return ""
	}
	defer procRegCloseKey.Call(key)
	n, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 1024)
	size := uint32(len(buf) * 2)
	if r, _, _ := procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(n)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

const (
	regFT = `Software\Freetrack\FreetrackClient`
	regNP = `Software\NaturalPoint\NATURALPOINT\NPClient Location`
)

// extract writes the client DLLs next to our settings so games can load them.
func (f *ftOutput) extract() (string, error) {
	local, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(local, "EDTrackerProTool", "ftclient")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, _ := ftFiles.ReadDir("ftfiles")
	for _, e := range entries {
		b, _ := ftFiles.ReadFile("ftfiles/" + e.Name())
		p := filepath.Join(dir, e.Name())
		if old, err := os.ReadFile(p); err == nil && len(old) == len(b) {
			continue // already there (may be in use by a running game)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %v", e.Name(), err)
		}
	}
	return dir, nil
}

func (f *ftOutput) Enable(startDummy bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled {
		return nil
	}
	dir, err := f.extract()
	if err != nil {
		f.err = err.Error()
		return err
	}
	name, _ := syscall.UTF16PtrFromString("FT_SharedMem")
	h, _, e := procCreateFileMapping.Call(invalidHandle, 0, pageReadWrite, 0, ftHeapSize, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		f.err = "shared memory: " + e.Error()
		return fmt.Errorf("%s", f.err)
	}
	base, _, e := procMapViewOfFile.Call(h, fileMapAll, 0, 0, ftHeapSize)
	if base == 0 {
		procCloseHandle.Call(h)
		f.err = "map view: " + e.Error()
		return fmt.Errorf("%s", f.err)
	}
	mname, _ := syscall.UTF16PtrFromString("FT_Mutext")
	mx, _, _ := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mname)))
	f.mapping, f.base, f.mutex, f.dir = h, base, mx, dir

	pathVal := strings.ReplaceAll(dir, `\`, "/") + "/"
	if err := regSetString(regFT, "Path", pathVal); err != nil {
		f.err = err.Error()
	}
	if err := regSetString(regNP, "Path", pathVal); err != nil {
		f.err = err.Error()
	}
	atomic.StoreUint32(f.u32(offDataID), 1)
	atomic.StoreUint32(f.u32(offCamW), 100)
	atomic.StoreUint32(f.u32(offCamH), 250)
	atomic.StoreUint32(f.u32(offGameID2), 0)
	atomic.StoreUint32(f.u32(offTable), 0)
	atomic.StoreUint32(f.u32(offTable+4), 0)
	f.lastGame, f.game = 0, ""
	if startDummy {
		cmd := exec.Command(filepath.Join(dir, "TrackIR.exe"))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Start(); err == nil {
			f.dummy = cmd
		}
	}
	f.enabled = true
	return nil
}

func (f *ftOutput) Disable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled {
		return
	}
	pathVal := strings.ReplaceAll(f.dir, `\`, "/") + "/"
	// only clear the registry if it still points at us (OpenTrack may have taken over)
	if regGetString(regFT, "Path") == pathVal {
		regSetString(regFT, "Path", "")
	}
	if regGetString(regNP, "Path") == pathVal {
		regSetString(regNP, "Path", "")
	}
	if f.dummy != nil && f.dummy.Process != nil {
		f.dummy.Process.Kill()
		f.dummy.Wait()
		f.dummy = nil
	}
	procUnmapViewOfFile.Call(f.base)
	procCloseHandle.Call(f.mapping)
	if f.mutex != 0 {
		procCloseHandle.Call(f.mutex)
	}
	f.base, f.mapping, f.mutex, f.enabled = 0, 0, 0, false
	f.game = ""
}

// Pose publishes yaw/pitch/roll in degrees (EDTracker joystick convention, after inversion).
// FreeTrack wants radians: yaw positive left, pitch positive up, roll positive left.
func (f *ftOutput) Pose(yawDeg, pitchDeg, rollDeg float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled || f.base == 0 {
		return
	}
	const d2r = math.Pi / 180
	y, p, r := float32(-yawDeg*d2r), float32(-pitchDeg*d2r), float32(rollDeg*d2r)
	f.storeF(offYaw, y)
	f.storeF(offPitch, p)
	f.storeF(offRoll, r)
	f.storeF(offRawYaw, y)
	f.storeF(offRawPitch, p)
	f.storeF(offRawRoll, r)
	for i := uintptr(0); i < 3; i++ {
		f.storeF(offX+4*i, 0)
	}
	id := int32(atomic.LoadUint32(f.u32(offGameID)))
	if id != f.lastGame {
		table, name := lookupGame(int(id))
		atomic.StoreUint32(f.u32(offTable), uint32(table[0])|uint32(table[1])<<8|uint32(table[2])<<16|uint32(table[3])<<24)
		atomic.StoreUint32(f.u32(offTable+4), uint32(table[4])|uint32(table[5])<<8|uint32(table[6])<<16|uint32(table[7])<<24)
		atomic.StoreUint32(f.u32(offGameID2), uint32(id))
		atomic.StoreUint32(f.u32(offDataID), 0)
		f.lastGame = id
		if id == 0 {
			f.game = ""
		} else if name == "" {
			f.game = fmt.Sprintf("unknown game (ID %d)", id)
		} else {
			f.game = fmt.Sprintf("%s (ID %d)", name, id)
		}
		if id != 0 {
			st.event("info", "FreeTrack: game connected — "+f.game)
		}
	} else {
		atomic.AddUint32(f.u32(offDataID), 1)
	}
	f.frames++
}

func (f *ftOutput) Status() (enabled bool, game, errStr string, frames uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled, f.game, f.err, f.frames
}

// lookupGame finds the TrackIR key table for a game ID in OpenTrack's supported-games list.
func lookupGame(id int) ([8]byte, string) {
	var table [8]byte
	if id == 0 {
		return table, ""
	}
	b, _ := ftFiles.ReadFile("ftfiles/games.csv")
	idStr := strconv.Itoa(id)
	for _, line := range strings.Split(string(b), "\n") {
		cols := strings.Split(strings.TrimRight(line, "\r"), ";")
		if len(cols) != 8 || cols[6] != idStr {
			continue
		}
		key := cols[7]
		if cols[3] != "V160" && len(key) == 22 {
			var raw [11]byte
			ok := true
			for i := 0; i < 11; i++ {
				v, err := strconv.ParseUint(key[2*i:2*i+2], 16, 8)
				if err != nil {
					ok = false
					break
				}
				raw[i] = byte(v)
			}
			if ok { // same byte order as OpenTrack's csv.cpp
				table = [8]byte{raw[5], raw[4], raw[3], raw[2], raw[9], raw[8], raw[7], raw[6]}
			}
		}
		return table, cols[1]
	}
	return table, ""
}
