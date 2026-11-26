//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	mutexName      = `Local\Duck.Mutex`
	stopEventName  = `Local\Duck.Stop`
	markerFileName = "hidden.marker"
	logFileName    = "duck.log"
	runSubKey      = `Software\Microsoft\Windows\CurrentVersion\Run`
	runKeyPath     = `HKCU\` + runSubKey
	runValueName   = "Duck"
	reassertLogGap = 10 * time.Second // at most one "came back" log line per gap
)

var exeDir string

func markerPath() string { return filepath.Join(exeDir, markerFileName) }

// hideTaskbars and showTaskbars act on the taskbars of one monitor handle. They
// leave the marker file alone: run() keeps it while any monitor is hidden.
func hideTaskbars(mon uintptr) { setVisible(taskbarsOn(mon), false) }

func showTaskbars(mon uintptr) { setVisible(taskbarsOn(mon), true) }

// showAllTaskbars shows every taskbar and removes the marker file. It is for
// crash recovery, --restore and "Quit and Restore taskbar".
func showAllTaskbars() {
	setVisible(taskbarWindows(), true)
	_ = os.Remove(markerPath())
}

// writeMarker records "taskbar hidden", so a crash while hidden can be undone
// on the next start.
func writeMarker() {
	_ = os.WriteFile(markerPath(), []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// ---------------------------------------------------------------- main loop

// monState is the hide state of one monitor's taskbar. The main loop keeps one
// per attached monitor (keyed by device name), so each taskbar is hidden,
// restored and re-hidden on its own and Duck never shows one it did not hide.
type monState struct {
	applied, pending bool          // state in effect / state waiting to be applied
	weHid            bool          // true only if THIS process hid this taskbar
	forcedHid        bool          // the applied hide came from Keep taskbar hidden
	hiddenFor        string        // process the taskbar is hidden for, for the tray tooltip
	pendingSince     time.Time     // when pending last changed
	pendingDelay     time.Duration // delay the pending state must hold before it is applied
}

// tick is what one pass of the main loop knows, shared by every monitor.
type tick struct {
	cfg       *Config
	fg        foreground
	paused    bool // Pause is on
	forced    bool // Keep taskbar hidden is on
	forcedNow bool // Keep taskbar hidden applies right now (not during shell UI)
	multi     bool // more than one monitor, so log lines name the monitor
}

var lastReassertLog time.Time

// desiredHidden says, for each monitor (by device name), whether its taskbar
// should be hidden right now.
func desiredHidden(mons []monInfo, t *tick) map[string]bool {
	want := make(map[string]bool, len(mons))
	if t.paused || !(t.fg.on || t.forcedNow) {
		return want // nothing hidden
	}
	// per_monitor limits a fullscreen app's hide to its own monitor. Keep
	// taskbar hidden always covers every monitor.
	only := t.cfg.PerMonitor && !t.forcedNow
	for _, m := range mons {
		want[m.device] = !only || m.handle == t.fg.mon
	}
	return want
}

// step moves one monitor toward want: it starts or advances the pending delay
// and applies the change once it has held long enough.
func (s *monState) step(m monInfo, want bool, t *tick) {
	where := ""
	if t.multi {
		where = " on " + m.shortName()
	}

	if want == s.applied {
		s.pending = s.applied
		// E.g. Keep taskbar hidden turned off while a fullscreen app is in
		// front: the taskbar stays hidden, now for the fullscreen app.
		s.forcedHid = s.applied && t.forcedNow
		if s.applied && !s.forcedHid {
			s.hiddenFor = t.fg.proc // e.g. one fullscreen app to another
		}
		// Explorer shows the taskbar again when it restarts and sometimes on
		// its own (e.g. opening Start). Hide it again while we own it.
		if s.applied && s.weHid && t.cfg.Reassert && anyVisible(taskbarsOn(m.handle)) {
			hideTaskbars(m.handle)
			if time.Since(lastReassertLog) > reassertLogGap {
				lastReassertLog = time.Now()
				log.Println("taskbar came back, hid it again" + where)
			}
		}
		return
	}
	if want != s.pending {
		s.pending = want
		s.pendingSince = time.Now()
	}
	switch {
	case want && t.forcedNow:
		s.pendingDelay = 0 // Keep taskbar hidden: hide now
	case want:
		s.pendingDelay = time.Duration(t.cfg.EnterDelayMs) * time.Millisecond
	case t.fg.shellUI || t.paused || (s.forcedHid && !t.forced):
		// Start, Search, a flyout, Pause, or Keep taskbar hidden turned
		// off: show the taskbar now.
		s.pendingDelay = 0
	default:
		s.pendingDelay = time.Duration(t.cfg.LeaveDelayMs) * time.Millisecond
	}
	if time.Since(s.pendingSince) < s.pendingDelay {
		return
	}

	// State has been stable long enough: apply it. Undo our previous hide first.
	if s.applied && s.weHid {
		showTaskbars(m.handle)
		s.weHid = false
		log.Println("taskbar restored" + where)
	}
	s.applied = want
	s.forcedHid = want && t.forcedNow
	if !want {
		return
	}
	ts := taskbarsOn(m.handle)
	switch {
	case len(ts) > 0 && !anyVisible(ts):
		log.Println("taskbar already hidden by something else, leaving it alone" + where)
	case len(ts) == 0 && len(taskbarWindows()) > 0:
		// This monitor has no taskbar of its own (e.g. "Show taskbar on all
		// displays" is off): nothing to hide here, and nothing to own.
	default:
		// If no taskbar exists yet (Explorer not running), still take
		// ownership so reassert hides it once Explorer comes up.
		hideTaskbars(m.handle)
		s.weHid = true
		if s.forcedHid {
			s.hiddenFor = ""
			log.Println("taskbar hidden (keep taskbar hidden is on)" + where)
		} else {
			s.hiddenFor = t.fg.proc
			log.Printf("taskbar hidden for %s (class %q)%s", t.fg.proc, t.fg.class, where)
		}
	}
}

func run(cfgPath string) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Printf("config problem, using defaults: %v", err)
	}
	cfgMod := modTime(cfgPath)

	stopEv, _, _ := pCreateEventW.Call(0, 1, 0, uintptr(unsafe.Pointer(utf16(stopEventName))))

	// Ctrl+C / SIGTERM (when run from a console) trigger the same clean exit as --quit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; pSetEvent.Call(stopEv) }()

	// Crash recovery: if a previous run died while the taskbar was hidden, restore it.
	if _, err := os.Stat(markerPath()); err == nil {
		log.Println("found stale marker from a previous run, restoring taskbar")
		showAllTaskbars()
	}

	states := map[string]*monState{} // hide state per monitor, by device name
	markerOn := false                // hidden.marker is on disk
	var lastMonKey string            // monitor list last written to the log
	var mons []monInfo               // monitors seen on the last tick
	var lastLogKey string
	lastCfgCheck := time.Now()

	// Wake the loop the moment the foreground window changes, instead of
	// waiting for the next poll. Polling stays on as a fallback for changes
	// that send no event (e.g. a window resizing into fullscreen).
	wakeEv, _, _ := pCreateEventW.Call(0, 0, 0, 0) // auto-reset
	stopWatch, hooked := startForegroundWatcher(wakeEv)
	defer stopWatch()
	if !hooked {
		log.Println("could not install the foreground hook, polling only")
	}

	// Keyboard shortcuts for Pause and Keep taskbar hidden (RegisterHotKey, not a keyboard hook).
	setHotkeys, stopHotkeys := startHotkeys(hotkeys{cfg.Hotkey, cfg.HideHotkey})
	defer stopHotkeys()

	// The tray icon. setTray starts or stops it; config reload calls it again
	// when tray_icon changes, so no restart is needed.
	var stopTray func() // nil while there is no tray icon
	setTray := func(on bool) {
		switch {
		case on && stopTray == nil:
			if stop, ok := startTray(stopEv); ok {
				stopTray = stop
			} else {
				log.Println("could not create the tray icon, running without it")
			}
		case !on && stopTray != nil:
			stopTray()
			stopTray = nil
		}
	}
	setTray(cfg.TrayIcon)
	defer func() { setTray(false) }()

	log.Printf("Duck %s watching (poll %dms, enter %dms, leave %dms, per_monitor %v, reassert %v)",
		versionString(), cfg.PollMs, cfg.EnterDelayMs, cfg.LeaveDelayMs, cfg.PerMonitor, cfg.Reassert)

	for {
		// Show the current state in the tray tooltip (posted only when it changes).
		hidden, hiddenFor := false, ""
		for _, m := range mons { // first hidden monitor names the app (primary first)
			if s := states[m.device]; s != nil && s.applied && s.weHid {
				hidden, hiddenFor = true, s.hiddenFor
				break
			}
		}
		setTrayTip(tipText(paused.Load(), forceHide.Load(), hidden, hiddenFor))

		// Wait one poll interval, or less if a pending state is due sooner, so
		// delays are not rounded up to the poll interval. Wake early on stop or
		// on a foreground change.
		timeout := cfg.PollMs
		for _, s := range states {
			if s.pending != s.applied {
				left := s.pendingDelay - time.Since(s.pendingSince)
				timeout = max(0, min(timeout, int((left+time.Millisecond-1)/time.Millisecond)))
			}
		}
		if waitStopOrWake(stopEv, wakeEv, timeout) {
			break
		}

		// Hot-reload config. The tray sets reloadConfig after editing the file,
		// so that change applies now instead of after the once-a-second check.
		if forced := reloadConfig.Swap(false); forced || time.Since(lastCfgCheck) > time.Second {
			lastCfgCheck = time.Now()
			if mt := modTime(cfgPath); forced || !mt.Equal(cfgMod) {
				cfgMod = mt
				if nc, err := loadConfig(cfgPath); err == nil {
					if nc.Hotkey != cfg.Hotkey || nc.HideHotkey != cfg.HideHotkey {
						setHotkeys(hotkeys{nc.Hotkey, nc.HideHotkey})
					}
					if nc.TrayIcon != cfg.TrayIcon {
						if nc.TrayIcon {
							log.Println("tray_icon turned on, adding the tray icon")
						} else {
							log.Println("tray_icon turned off, removing the tray icon")
						}
						setTray(nc.TrayIcon)
					}
					cfg = nc
					log.Println("config reloaded")
				} else {
					log.Printf("config reload failed, keeping old settings: %v", err)
				}
			}
		}

		fg := detectForeground(&cfg)
		// Log each foreground change (not rect changes, to keep dragging quiet).
		if key := fmt.Sprintf("%x|%v|%v|%s", fg.hwnd, fg.on, fg.shellUI, fg.note); fg.hwnd != 0 && key != lastLogKey {
			lastLogKey = key
			log.Printf("foreground: %s", fg)
		}
		// Remember the last fullscreen app for "Never hide for ..." in the tray
		// menu. Opening the menu makes Duck the foreground window, so the tray
		// cannot look this up itself at that point.
		if fg.on {
			if last := lastFullscreen.Load(); last == nil || *last != fg.proc {
				proc := fg.proc
				lastFullscreen.Store(&proc)
			}
		}

		// "Keep taskbar hidden" hides every taskbar whatever is in front, except
		// while Start, Search or a flyout is open. Pause wins over it.
		isPaused := paused.Load()
		isForced := forceHide.Load()
		forcedNow := isForced && !fg.shellUI
		t := &tick{cfg: &cfg, fg: fg, paused: isPaused, forced: isForced, forcedNow: forcedNow}

		// Look at the monitors every tick (cheap) and log the list when it changes.
		// If the call fails, keep the last list rather than forgetting our state.
		if list := listMonitors(false); len(list) > 0 {
			mons = list
			if key := monitorsKey(mons); key != lastMonKey {
				lastMonKey = key
				log.Printf("monitors: %s", describeMonitors(listMonitors(true)))
			}
			for dev := range states { // a monitor that went away takes its taskbar with it
				if !slices.ContainsFunc(mons, func(m monInfo) bool { return m.device == dev }) {
					delete(states, dev)
				}
			}
		}
		t.multi = len(mons) > 1

		want := desiredHidden(mons, t)
		for _, m := range mons {
			s := states[m.device]
			if s == nil {
				s = &monState{}
				states[m.device] = s
			}
			s.step(m, want[m.device], t)
		}

		// hidden.marker exists while any taskbar is hidden by us.
		anyHid := false
		for _, s := range states {
			anyHid = anyHid || s.weHid
		}
		switch {
		case anyHid && !markerOn:
			writeMarker()
			markerOn = true
		case !anyHid && markerOn:
			_ = os.Remove(markerPath())
			markerOn = false
		}
	}

	// Clean exit: never leave the taskbar hidden behind us.
	if restoreOnExit.Load() {
		// "Quit and Restore taskbar" from the tray: same as --restore.
		log.Println("exiting, forcing every taskbar to show")
		showAllTaskbars()
	} else {
		restored := false
		for _, m := range listMonitors(false) {
			if s := states[m.device]; s != nil && s.weHid {
				showTaskbars(m.handle)
				restored = true
			}
		}
		if restored {
			log.Println("exiting, restoring taskbar")
		}
		_ = os.Remove(markerPath())
	}
	log.Println("stopped")
}

// ---------------------------------------------------------------- commands

func singleInstance() bool {
	h, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(utf16(mutexName))))
	if h == 0 {
		return true // could not create; do not block startup
	}
	if errno, ok := err.(syscall.Errno); ok && errno == errorAlreadyExists {
		return false
	}
	return true // handle intentionally kept open for the life of the process
}

func sendStop() {
	h, _, _ := pOpenEventW.Call(eventModifyState, 0, uintptr(unsafe.Pointer(utf16(stopEventName))))
	if h == 0 {
		log.Println("quit: no running instance found")
		return
	}
	pSetEvent.Call(h)
	pCloseHandle.Call(h)
	log.Println("quit: stop signal sent")
}

func setAutostart(enable bool) {
	var cmd *exec.Cmd
	if enable {
		exe, _ := os.Executable()
		cmd = exec.Command("reg", "add", runKeyPath, "/v", runValueName, "/t", "REG_SZ", "/d", `"`+exe+`"`, "/f")
	} else {
		cmd = exec.Command("reg", "delete", runKeyPath, "/v", runValueName, "/f")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("autostart change failed: %v %s", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("autostart enabled=%v", enable)
}

func openLog() {
	path := filepath.Join(exeDir, logFileName)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		_ = os.Remove(path) // simple size cap: start fresh past 1 MB
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.Ldate | log.Ltime)
}

func main() {
	exe, _ := os.Executable()
	exeDir = filepath.Dir(exe)
	openLog()
	pSetProcessDPIAware.Call() // so window and monitor rects use the same coordinates

	cfgPath := filepath.Join(exeDir, "config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		writeDefaultConfig(cfgPath)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--quit", "--stop": // --stop is the old name, kept so existing shortcuts still work
			sendStop()
		case "--restore":
			// A running watcher would hide the taskbar again, so stop it first.
			sendStop()
			showAllTaskbars()
			log.Println("--restore: taskbar shown")
		case "--install-autostart":
			setAutostart(true)
		case "--remove-autostart":
			setAutostart(false)
		case "--version":
			showVersion()
		default:
			log.Printf("unknown option %q (use --quit, --restore, --install-autostart, --remove-autostart, --version)", os.Args[1])
		}
		return
	}

	if !singleInstance() {
		if notifyRunningInstance() {
			log.Println("another instance is already running, asked it to show a notification, exiting")
		} else {
			log.Println("another instance is already running (without a tray icon), exiting")
		}
		return
	}
	run(cfgPath)
}
