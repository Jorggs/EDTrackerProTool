//go:build windows

package main

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Low-level keyboard hook, as the original EDTracker UI used (WH_KEYBOARD_LL).
// It sees key presses even while a game has focus; GetAsyncKeyState is kept as a fallback.

var (
	procSetWindowsHookExW = modUser32.NewProc("SetWindowsHookExW")
	procCallNextHookEx    = modUser32.NewProc("CallNextHookEx")
	procGetMessageW       = modUser32.NewProc("GetMessageW")
	procGetModuleHandleW  = modKernel32.NewProc("GetModuleHandleW")

	hookMu     sync.Mutex
	hookDown   [256]bool
	hookActive bool
)

type kbdLLHook struct {
	VkCode, ScanCode, Flags, Time uint32
	ExtraInfo                     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
	_       uint32
}

func hookKeyDown(vk int) bool {
	hookMu.Lock()
	defer hookMu.Unlock()
	if !hookActive || vk < 0 || vk > 255 {
		return false
	}
	switch vk { // generic modifiers from left/right variants
	case 0x10:
		return hookDown[0x10] || hookDown[0xA0] || hookDown[0xA1]
	case 0x11:
		return hookDown[0x11] || hookDown[0xA2] || hookDown[0xA3]
	case 0x12:
		return hookDown[0x12] || hookDown[0xA4] || hookDown[0xA5]
	}
	return hookDown[vk]
}

func startKeyboardHook() {
	ready := make(chan bool)
	go func() {
		runtime.LockOSThread()
		cb := syscall.NewCallback(func(nCode, wParam, lParam uintptr) uintptr {
			if int32(nCode) == 0 {
				k := (*kbdLLHook)(unsafe.Pointer(lParam))
				vk := k.VkCode & 0xFF
				hookMu.Lock()
				switch wParam {
				case 0x100, 0x104: // WM_KEYDOWN, WM_SYSKEYDOWN
					hookDown[vk] = true
				case 0x101, 0x105: // WM_KEYUP, WM_SYSKEYUP
					hookDown[vk] = false
				}
				hookMu.Unlock()
			}
			r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
			return r
		})
		hmod, _, _ := procGetModuleHandleW.Call(0)
		h, _, err := procSetWindowsHookExW.Call(13, cb, hmod, 0) // WH_KEYBOARD_LL
		hookMu.Lock()
		hookActive = h != 0
		hookMu.Unlock()
		if h == 0 {
			st.event("warn", "Keyboard hook failed ("+err.Error()+"), using key polling only")
		}
		ready <- h != 0
		var m winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
		}
	}()
	<-ready
}
