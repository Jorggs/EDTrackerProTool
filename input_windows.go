//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modUser32 = syscall.NewLazyDLL("user32.dll")
	modWinmm  = syscall.NewLazyDLL("winmm.dll")

	procGetAsyncKeyState = modUser32.NewProc("GetAsyncKeyState")
	procGetKeyNameTextW  = modUser32.NewProc("GetKeyNameTextW")
	procMapVirtualKeyW   = modUser32.NewProc("MapVirtualKeyW")
	procMessageBeep      = modUser32.NewProc("MessageBeep")
	procJoyGetNumDevs    = modWinmm.NewProc("joyGetNumDevs")
	procJoyGetDevCapsW   = modWinmm.NewProc("joyGetDevCapsW")
	procJoyGetPosEx      = modWinmm.NewProc("joyGetPosEx")
)

// Binding is a user-assigned trigger: a keyboard combination or a joystick button.
type Binding struct {
	Type   string `json:"type"` // "", "key" or "joy"
	VK     int    `json:"vk,omitempty"`
	Ctrl   bool   `json:"ctrl,omitempty"`
	Shift  bool   `json:"shift,omitempty"`
	Alt    bool   `json:"alt,omitempty"`
	JoyID  int    `json:"joyId,omitempty"`
	JoyMid uint16 `json:"joyMid,omitempty"` // vendor/product of that joystick, to re-find it if IDs shift
	JoyPid uint16 `json:"joyPid,omitempty"`
	Button int    `json:"button,omitempty"`
	Label  string `json:"label,omitempty"`
}

type joyInfoEx struct {
	Size, Flags, X, Y, Z, R, U, V, Buttons, ButtonNumber, POV, Res1, Res2 uint32
}

const joyReturnAll = 0xFF

func keyDown(vk int) bool {
	if hookKeyDown(vk) {
		return true
	}
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func joyState(id int) (joyInfoEx, bool) {
	var ji joyInfoEx
	ji.Size = uint32(unsafe.Sizeof(ji))
	ji.Flags = joyReturnAll
	r, _, _ := procJoyGetPosEx.Call(uintptr(id), uintptr(unsafe.Pointer(&ji)))
	return ji, r == 0
}

type joyCaps struct {
	Mid, Pid uint16
	Name     string
}

func joyGetCaps(id int) (joyCaps, bool) {
	buf := make([]byte, 1024) // JOYCAPSW is ~728 bytes
	r, _, _ := procJoyGetDevCapsW.Call(uintptr(id), uintptr(unsafe.Pointer(&buf[0])), 728)
	if r != 0 {
		return joyCaps{}, false
	}
	mid := *(*uint16)(unsafe.Pointer(&buf[0]))
	pid := *(*uint16)(unsafe.Pointer(&buf[2]))
	name := (*[32]uint16)(unsafe.Pointer(&buf[4]))
	return joyCaps{mid, pid, syscall.UTF16ToString(name[:])}, true
}

func joyCount() int {
	n, _, _ := procJoyGetNumDevs.Call()
	if n > 16 {
		n = 16
	}
	return int(n)
}

func keyName(vk int) string {
	sc, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	lp := sc << 16
	switch vk { // extended keys
	case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2D, 0x2E, 0x6F, 0x90:
		lp |= 1 << 24
	}
	buf := make([]uint16, 64)
	n, _, _ := procGetKeyNameTextW.Call(lp, uintptr(unsafe.Pointer(&buf[0])), 64)
	if n > 0 {
		return syscall.UTF16ToString(buf[:n])
	}
	return fmt.Sprintf("Key 0x%02X", vk)
}

func (b Binding) describe() string {
	switch b.Type {
	case "key":
		s := ""
		if b.Ctrl {
			s += "Ctrl+"
		}
		if b.Shift {
			s += "Shift+"
		}
		if b.Alt {
			s += "Alt+"
		}
		return s + keyName(b.VK)
	case "joy":
		return fmt.Sprintf("%s button %d", b.Label, b.Button+1)
	}
	return "Not set"
}

func modsDown() (ctrl, shift, alt bool) {
	return keyDown(0x11), keyDown(0x10), keyDown(0x12)
}

func isModifierVK(vk int) bool {
	return vk == 0x10 || vk == 0x11 || vk == 0x12 || (vk >= 0xA0 && vk <= 0xA5) || vk == 0x5B || vk == 0x5C
}

// resolveJoy finds the current winmm ID for a bound joystick (IDs can shift when devices are replugged).
func resolveJoy(b *Binding) int {
	if c, ok := joyGetCaps(b.JoyID); ok && c.Mid == b.JoyMid && c.Pid == b.JoyPid {
		return b.JoyID
	}
	for i := 0; i < joyCount(); i++ {
		if c, ok := joyGetCaps(i); ok && c.Mid == b.JoyMid && c.Pid == b.JoyPid {
			b.JoyID = i
			return i
		}
	}
	return b.JoyID
}

// pressed reports whether the binding is currently held.
func (b *Binding) pressed() bool {
	switch b.Type {
	case "key":
		if !keyDown(b.VK) {
			return false
		}
		c, s, a := modsDown()
		return c == b.Ctrl && s == b.Shift && a == b.Alt
	case "joy":
		ji, ok := joyState(resolveJoy(b))
		return ok && ji.Buttons&(1<<uint(b.Button)) != 0
	}
	return false
}

// captureSnapshot records what is already held so capture only reacts to new presses.
type captureSnapshot struct {
	keys [256]bool
	joy  [16]uint32
}

func takeSnapshot() captureSnapshot {
	var s captureSnapshot
	for vk := 1; vk < 256; vk++ {
		s.keys[vk] = keyDown(vk)
	}
	for i := 0; i < joyCount(); i++ {
		if ji, ok := joyState(i); ok {
			s.joy[i] = ji.Buttons
		}
	}
	return s
}

// pollCapture returns a binding if a new key or joystick button was pressed since snap.
// Escape returns an empty binding with ok=true (cancel).
func pollCapture(snap *captureSnapshot) (Binding, bool) {
	for vk := 8; vk < 256; vk++ {
		down := keyDown(vk)
		if down && !snap.keys[vk] && !isModifierVK(vk) && vk > 6 {
			if vk == 0x1B {
				return Binding{}, true
			}
			c, s, a := modsDown()
			b := Binding{Type: "key", VK: vk, Ctrl: c, Shift: s, Alt: a}
			b.Label = b.describe()
			return b, true
		}
		snap.keys[vk] = down
	}
	for i := 0; i < joyCount(); i++ {
		ji, ok := joyState(i)
		if !ok {
			continue
		}
		newBits := ji.Buttons &^ snap.joy[i]
		snap.joy[i] = ji.Buttons
		if newBits != 0 {
			btn := 0
			for newBits&1 == 0 {
				newBits >>= 1
				btn++
			}
			caps, _ := joyGetCaps(i)
			name := caps.Name
			if name == "" {
				name = fmt.Sprintf("Joystick %d", i+1)
			}
			b := Binding{Type: "joy", JoyID: i, JoyMid: caps.Mid, JoyPid: caps.Pid, Button: btn, Label: name}
			b.Label = name
			return b, true
		}
	}
	return Binding{}, false
}

// trackerJoystick finds the EDTracker among winmm joysticks (fallback for live angles).
func trackerJoystick(vid, pid uint16) int {
	for i := 0; i < joyCount(); i++ {
		if c, ok := joyGetCaps(i); ok && c.Mid == vid && c.Pid == pid {
			return i
		}
	}
	return -1
}

func beep() { procMessageBeep.Call(0xFFFFFFFF) }
