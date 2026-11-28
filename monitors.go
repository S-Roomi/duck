//go:build windows

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// monInfo describes one attached monitor. The handle changes when displays are
// replugged or the display configuration changes, so it is only good for the
// current moment. device (\\.\DISPLAY2) and hwid are what a config file can name.
type monInfo struct {
	handle  uintptr
	device  string // GDI device name, e.g. \\.\DISPLAY2
	primary bool
	rect    rect
	hwid    string // e.g. MONITOR\GSM5B09; only filled when asked for
}

// shortName is the device name without the \\.\ prefix, e.g. DISPLAY2.
func (m monInfo) shortName() string { return strings.TrimPrefix(m.device, `\\.\`) }

func (m monInfo) String() string {
	s := fmt.Sprintf("%s %dx%d", m.shortName(), m.rect.Right-m.rect.Left, m.rect.Bottom-m.rect.Top)
	if m.primary {
		s += " primary"
	}
	if m.hwid != "" {
		s += " " + m.hwid
	}
	return s
}

// The EnumDisplayMonitors callback is created once (callback slots are never
// freed) and appends to enumOut, which listMonitors guards with enumMu.
var (
	enumMu  sync.Mutex
	enumOut []monInfo
)

var enumMonitorsCb = syscall.NewCallback(func(hmon, hdc, rc, lparam uintptr) uintptr {
	mi := monitorInfoEx{}
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	if ok, _, _ := pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
		enumOut = append(enumOut, monInfo{
			handle:  hmon,
			device:  syscall.UTF16ToString(mi.SzDevice[:]),
			primary: mi.DwFlags&monitorInfoPrimary != 0,
			rect:    mi.RcMonitor,
		})
	}
	return 1 // keep enumerating
})

// listMonitors returns the attached monitors, primary first, then by device
// name. withHWID also looks up each monitor's hardware ID, which costs one
// extra call per monitor, so the main loop leaves it off.
func listMonitors(withHWID bool) []monInfo {
	enumMu.Lock()
	enumOut = nil
	pEnumDisplayMonitors.Call(0, 0, enumMonitorsCb, 0)
	list := enumOut
	enumOut = nil
	enumMu.Unlock()

	sort.Slice(list, func(i, j int) bool {
		if list[i].primary != list[j].primary {
			return list[i].primary
		}
		return list[i].device < list[j].device
	})
	if withHWID {
		for i := range list {
			list[i].hwid = hardwareID(list[i].device)
		}
	}
	return list
}

// hardwareID returns the monitor's PnP hardware ID (manufacturer + product,
// e.g. MONITOR\GSM5B09) for the display device name, or "" if unknown. Two
// monitors of the same model share one ID.
func hardwareID(device string) string {
	var dd displayDevice
	dd.Cb = uint32(unsafe.Sizeof(dd))
	if r, _, _ := pEnumDisplayDevicesW.Call(uintptr(unsafe.Pointer(utf16(device))), 0, uintptr(unsafe.Pointer(&dd)), 0); r == 0 {
		return ""
	}
	// DeviceID looks like MONITOR\GSM5B09\{4d36e96e-...}\0001.
	parts := strings.Split(syscall.UTF16ToString(dd.DeviceID[:]), `\`)
	if len(parts) < 2 || !strings.EqualFold(parts[0], "MONITOR") {
		return ""
	}
	return strings.ToUpper(parts[0] + `\` + parts[1])
}

// matchMonitor reports whether the config entry spec names monitor m:
//
//	"primary"            the primary monitor (stable across replugging)
//	"secondary"          every monitor that is not the primary one
//	"DISPLAY2"           the GDI device name, with or without the \\.\ prefix
//	"MONITOR\GSM5B09"    the hardware ID (m.hwid must be filled in)
//
// Matching ignores case.
func matchMonitor(spec string, m monInfo) bool {
	spec = strings.TrimSpace(spec)
	switch strings.ToLower(spec) {
	case "":
		return false
	case "primary":
		return m.primary
	case "secondary":
		return !m.primary
	}
	if strings.EqualFold(strings.TrimPrefix(spec, `\\.\`), m.shortName()) {
		return true
	}
	return m.hwid != "" && strings.EqualFold(spec, m.hwid)
}

// matchAnyMonitor reports whether any entry of specs names m.
func matchAnyMonitor(specs []string, m monInfo) bool {
	for _, s := range specs {
		if matchMonitor(s, m) {
			return true
		}
	}
	return false
}

// monitorByHandle finds the monitor with the given handle in list.
func monitorByHandle(list []monInfo, h uintptr) (monInfo, bool) {
	for _, m := range list {
		if m.handle == h {
			return m, true
		}
	}
	return monInfo{}, false
}

// describeMonitors is the one-line list written to the log.
func describeMonitors(list []monInfo) string {
	parts := make([]string, len(list))
	for i, m := range list {
		parts[i] = m.String()
	}
	return strings.Join(parts, ", ")
}

// monitorsKey changes whenever a monitor is added, removed or moved, so the
// main loop can log the list only when it changes.
func monitorsKey(list []monInfo) string {
	var b strings.Builder
	for _, m := range list {
		fmt.Fprintf(&b, "%s|%v|%v;", m.device, m.primary, m.rect)
	}
	return b.String()
}
