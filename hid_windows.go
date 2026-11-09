//go:build windows

package main

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modSetupapi = syscall.NewLazyDLL("setupapi.dll")
	modHid      = syscall.NewLazyDLL("hid.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetupDiGetClassDevsW             = modSetupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = modSetupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = modSetupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = modSetupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procHidDGetHidGuid        = modHid.NewProc("HidD_GetHidGuid")
	procHidDGetAttributes     = modHid.NewProc("HidD_GetAttributes")
	procHidDGetPreparsedData  = modHid.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData = modHid.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps           = modHid.NewProc("HidP_GetCaps")
	procHidDGetFeature        = modHid.NewProc("HidD_GetFeature")
	procHidDSetFeature        = modHid.NewProc("HidD_SetFeature")
	procHidDGetInputReport    = modHid.NewProc("HidD_GetInputReport")
	procHidDGetProductString  = modHid.NewProc("HidD_GetProductString")

	procCreateFileW = modKernel32.NewProc("CreateFileW")
	procCloseHandle = modKernel32.NewProc("CloseHandle")
)

const (
	digcfPresent         = 0x2
	digcfDeviceInterface = 0x10
	genericRead          = 0x80000000
	genericWrite         = 0x40000000
	fileShareRead        = 0x1
	fileShareWrite       = 0x2
	openExisting         = 3
	invalidHandle        = ^uintptr(0)
	hidpStatusSuccess    = 0x00110000
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type spDeviceInterfaceData struct {
	CbSize   uint32
	Guid     guid
	Flags    uint32
	Reserved uintptr
}

type hiddAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
	_             uint16
}

type hidpCaps struct {
	Usage                   uint16
	UsagePage               uint16
	InputReportByteLength   uint16
	OutputReportByteLength  uint16
	FeatureReportByteLength uint16
	Reserved                [17]uint16
	Rest                    [10]uint16
}

// Known EDTracker USB IDs (from the original EDTracker Pro UI).
var knownIDs = []struct{ vid, pid uint16 }{
	{0x16D0, 0x0AEA}, {0x16D0, 0x0AEB}, {0x16D0, 0x0AEC}, {0x16D0, 0x0C41},
	{0x16D0, 0x0C42}, {0x1915, 0x9005}, {0x03EB, 0x2044},
}

type hidDevice struct {
	mu         sync.Mutex
	handle     uintptr
	Path       string
	VendorID   uint16
	ProductID  uint16
	Product    string
	FeatureLen int
	InputLen   int
}

func isKnown(vid, pid uint16) bool {
	for _, k := range knownIDs {
		if k.vid == vid && k.pid == pid {
			return true
		}
	}
	return false
}

func hidDevicePaths() ([]string, error) {
	var g guid
	procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&g)))
	h, _, err := procSetupDiGetClassDevsW.Call(uintptr(unsafe.Pointer(&g)), 0, 0, digcfPresent|digcfDeviceInterface)
	if h == invalidHandle {
		return nil, fmt.Errorf("SetupDiGetClassDevs: %v", err)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(h)
	var paths []string
	for i := 0; ; i++ {
		var did spDeviceInterfaceData
		did.CbSize = uint32(unsafe.Sizeof(did))
		r, _, _ := procSetupDiEnumDeviceInterfaces.Call(h, 0, uintptr(unsafe.Pointer(&g)), uintptr(i), uintptr(unsafe.Pointer(&did)))
		if r == 0 {
			break
		}
		var need uint32
		procSetupDiGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&did)), 0, 0, uintptr(unsafe.Pointer(&need)), 0)
		if need == 0 {
			continue
		}
		buf := make([]byte, need+8)
		cb := uint32(8) // sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W) on 64-bit
		if unsafe.Sizeof(uintptr(0)) == 4 {
			cb = 6
		}
		*(*uint32)(unsafe.Pointer(&buf[0])) = cb
		r, _, _ = procSetupDiGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&did)), uintptr(unsafe.Pointer(&buf[0])), uintptr(need), 0, 0)
		if r == 0 {
			continue
		}
		u16 := (*[1 << 14]uint16)(unsafe.Pointer(&buf[4]))[: (need-4)/2 : (need-4)/2]
		paths = append(paths, syscall.UTF16ToString(u16))
	}
	return paths, nil
}

func openPath(path string, access uint32) uintptr {
	p, _ := syscall.UTF16PtrFromString(path)
	h, _, _ := procCreateFileW.Call(uintptr(unsafe.Pointer(p)), uintptr(access), fileShareRead|fileShareWrite, 0, openExisting, 0, 0)
	return h
}

// findTracker returns the EDTracker HID interface that carries the feature reports.
func findTracker() (*hidDevice, error) {
	paths, err := hidDevicePaths()
	if err != nil {
		return nil, err
	}
	var best *hidDevice
	for _, path := range paths {
		h := openPath(path, 0) // attributes only, works even for exclusive devices
		if h == invalidHandle {
			continue
		}
		var a hiddAttributes
		a.Size = uint32(unsafe.Sizeof(a))
		r, _, _ := procHidDGetAttributes.Call(h, uintptr(unsafe.Pointer(&a)))
		procCloseHandle.Call(h)
		if r == 0 || !isKnown(a.VendorID, a.ProductID) {
			continue
		}
		h = openPath(path, genericRead|genericWrite)
		if h == invalidHandle {
			continue
		}
		var pp uintptr
		var caps hidpCaps
		if r, _, _ := procHidDGetPreparsedData.Call(h, uintptr(unsafe.Pointer(&pp))); r != 0 {
			s, _, _ := procHidPGetCaps.Call(pp, uintptr(unsafe.Pointer(&caps)))
			procHidDFreePreparsedData.Call(pp)
			if uint32(s) != hidpStatusSuccess {
				caps = hidpCaps{}
			}
		}
		name := make([]uint16, 128)
		procHidDGetProductString.Call(h, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)*2))
		d := &hidDevice{handle: h, Path: path, VendorID: a.VendorID, ProductID: a.ProductID,
			Product: syscall.UTF16ToString(name), FeatureLen: int(caps.FeatureReportByteLength), InputLen: int(caps.InputReportByteLength)}
		if best == nil || d.FeatureLen > best.FeatureLen {
			if best != nil {
				best.Close()
			}
			best = d
		} else {
			d.Close()
		}
	}
	if best == nil {
		return nil, errors.New("EDTracker not found")
	}
	if best.FeatureLen == 0 {
		best.FeatureLen = 45 // matches the original UI's buffer size
	}
	return best, nil
}

func (d *hidDevice) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle != 0 && d.handle != invalidHandle {
		procCloseHandle.Call(d.handle)
	}
	d.handle = 0
}

// GetFeature returns the full report buffer, byte 0 = report ID (Windows semantics).
func (d *hidDevice) GetFeature(id byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return nil, errors.New("device closed")
	}
	buf := make([]byte, d.FeatureLen)
	buf[0] = id
	r, _, err := procHidDGetFeature.Call(d.handle, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return nil, fmt.Errorf("HidD_GetFeature(%d): %v", id, err)
	}
	return buf, nil
}

// SetFeature sends payload (without report ID), padded to the device's feature length.
func (d *hidDevice) SetFeature(id byte, payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return errors.New("device closed")
	}
	buf := make([]byte, d.FeatureLen)
	buf[0] = id
	copy(buf[1:], payload)
	r, _, err := procHidDSetFeature.Call(d.handle, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return fmt.Errorf("HidD_SetFeature(%d): %v", id, err)
	}
	return nil
}

// GetJoystick reads input report 1 (yaw, pitch, roll as int16) directly.
func (d *hidDevice) GetJoystick() ([3]int16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out [3]int16
	if d.handle == 0 || d.InputLen < 7 {
		return out, errors.New("no input report")
	}
	buf := make([]byte, d.InputLen)
	buf[0] = 1
	r, _, err := procHidDGetInputReport.Call(d.handle, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return out, err
	}
	for i := 0; i < 3; i++ {
		out[i] = int16(uint16(buf[1+2*i]) | uint16(buf[2+2*i])<<8)
	}
	return out, nil
}
