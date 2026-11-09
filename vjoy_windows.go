//go:build windows

package main

// vJoy virtual joystick output via vJoyInterface.dll (installed with the vJoy driver).
// Yaw, pitch and roll go to three axes, scaled ±180° → the axis's full range,
// the same scaling the EDTracker's own joystick uses.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	vjdStatOwn  = 0
	vjdStatFree = 1
	vjdStatBusy = 2
	vjdStatMiss = 3
)

var vjoyAxisSets = map[string][3]uint32{
	"xyz":    {0x30, 0x31, 0x32}, // X, Y, Z
	"rxryrz": {0x33, 0x34, 0x35}, // Rx, Ry, Rz
}

type vjoyOutput struct {
	mu       sync.Mutex
	dll      *syscall.DLL
	procs    map[string]*syscall.Proc
	enabled  bool
	id       uint32
	axes     [3]uint32
	min, max [3]int32
	err      string
	dllPath  string
}

var vjoy = &vjoyOutput{}

func vjoyDLLCandidates() []string {
	var c []string
	if exe, err := os.Executable(); err == nil {
		c = append(c, filepath.Join(filepath.Dir(exe), "vJoyInterface.dll"))
	}
	for _, env := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		if pf := os.Getenv(env); pf != "" {
			c = append(c, filepath.Join(pf, "vJoy", "x64", "vJoyInterface.dll"), filepath.Join(pf, "vJoy", "vJoyInterface.dll"))
		}
	}
	return c
}

func (v *vjoyOutput) load() error {
	if v.dll != nil {
		return nil
	}
	var lastErr error
	for _, p := range vjoyDLLCandidates() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		d, err := syscall.LoadDLL(p)
		if err != nil {
			lastErr = err
			continue
		}
		procs := map[string]*syscall.Proc{}
		for _, n := range []string{"vJoyEnabled", "GetVJDStatus", "AcquireVJD", "RelinquishVJD", "SetAxis", "GetVJDAxisExist", "GetVJDAxisMax", "GetVJDAxisMin", "ResetVJD"} {
			pr, err := d.FindProc(n)
			if err != nil && n != "GetVJDAxisMin" { // Min is missing in old SDKs
				d.Release()
				return fmt.Errorf("%s: missing %s", p, n)
			}
			procs[n] = pr
		}
		v.dll, v.procs, v.dllPath = d, procs, p
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("couldn't load vJoyInterface.dll: %v", lastErr)
	}
	return fmt.Errorf("vJoyInterface.dll not found — is vJoy installed? (looked in Program Files\\vJoy\\x64 and next to this exe)")
}

func (v *vjoyOutput) call(name string, args ...uintptr) uintptr {
	p := v.procs[name]
	if p == nil {
		return 0
	}
	r, _, _ := p.Call(args...)
	return r
}

func (v *vjoyOutput) Enable(id int, axisSet string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.disableLocked()
	if id < 1 || id > 16 {
		id = 1
	}
	axes, ok := vjoyAxisSets[axisSet]
	if !ok {
		axes = vjoyAxisSets["xyz"]
	}
	fail := func(e error) error { v.err = e.Error(); return e }
	if err := v.load(); err != nil {
		return fail(err)
	}
	if v.call("vJoyEnabled") == 0 {
		return fail(fmt.Errorf("vJoy driver is installed but not enabled (check 'Configure vJoy')"))
	}
	switch int32(v.call("GetVJDStatus", uintptr(id))) {
	case vjdStatBusy:
		return fail(fmt.Errorf("vJoy device %d is in use by another program (OpenTrack's vJoy output, perhaps)", id))
	case vjdStatMiss:
		return fail(fmt.Errorf("vJoy device %d doesn't exist — add it in 'Configure vJoy'", id))
	}
	if v.call("AcquireVJD", uintptr(id)) == 0 {
		return fail(fmt.Errorf("couldn't acquire vJoy device %d", id))
	}
	names := []string{"X", "Y", "Z", "Rx", "Ry", "Rz"}
	for i, a := range axes {
		if v.call("GetVJDAxisExist", uintptr(id), uintptr(a)) == 0 {
			v.call("RelinquishVJD", uintptr(id))
			return fail(fmt.Errorf("vJoy device %d has no %s axis — enable it in 'Configure vJoy' or pick the other axis set", id, names[a-0x30]))
		}
		var mx, mn int32 = 32767, 0
		v.call("GetVJDAxisMax", uintptr(id), uintptr(a), uintptr(unsafe.Pointer(&mx)))
		if v.procs["GetVJDAxisMin"] != nil {
			v.call("GetVJDAxisMin", uintptr(id), uintptr(a), uintptr(unsafe.Pointer(&mn)))
		}
		if mx <= mn {
			mn, mx = 0, 32767
		}
		v.min[i], v.max[i] = mn, mx
	}
	v.id, v.axes, v.enabled, v.err = uint32(id), axes, true, ""
	v.poseLocked(0, 0, 0)
	return nil
}

func (v *vjoyOutput) disableLocked() {
	if v.enabled {
		v.poseLocked(0, 0, 0)
		v.call("RelinquishVJD", uintptr(v.id))
	}
	v.enabled = false
}

func (v *vjoyOutput) Disable() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.disableLocked()
	v.err = ""
}

func (v *vjoyOutput) poseLocked(yaw, pitch, roll float64) {
	for i, deg := range [3]float64{yaw, pitch, roll} {
		if deg > 180 {
			deg = 180
		} else if deg < -180 {
			deg = -180
		}
		val := float64(v.min[i]) + (deg+180)/360*float64(v.max[i]-v.min[i])
		v.call("SetAxis", uintptr(int32(val+0.5)), uintptr(v.id), uintptr(v.axes[i]))
	}
}

// Pose sets the axes from degrees (after inversion).
func (v *vjoyOutput) Pose(yaw, pitch, roll float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.enabled {
		v.poseLocked(yaw, pitch, roll)
	}
}

func (v *vjoyOutput) Status() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	return map[string]any{"vjoy": v.enabled, "vjoyErr": v.err, "vjoyDevice": v.id}
}
