//go:build windows

package main

import "testing"

func TestMatchMonitor(t *testing.T) {
	d1 := monInfo{device: `\\.\DISPLAY1`, primary: true, hwid: `MONITOR\GSM5B09`}
	d2 := monInfo{device: `\\.\DISPLAY2`, hwid: `MONITOR\DEL4123`}
	d3 := monInfo{device: `\\.\DISPLAY3`} // no hardware ID known

	cases := []struct {
		spec string
		m    monInfo
		want bool
	}{
		{"primary", d1, true},
		{"PRIMARY", d1, true},
		{"primary", d2, false},
		{"secondary", d1, false},
		{"secondary", d2, true},
		{"secondary", d3, true}, // every non-primary monitor
		{"DISPLAY2", d2, true},
		{"display2", d2, true},
		{`\\.\DISPLAY2`, d2, true},
		{`\\.\display2`, d2, true},
		{"DISPLAY2", d1, false},
		{"DISPLAY1", d2, false},
		{"DISPLAY", d1, false}, // not a prefix match
		{`MONITOR\DEL4123`, d2, true},
		{`monitor\del4123`, d2, true},
		{`MONITOR\DEL4123`, d1, false},
		{`MONITOR\DEL4123`, d3, false},
		{"  primary ", d1, true},
		{"", d1, false},
		{"", d3, false},
	}
	for _, c := range cases {
		if got := matchMonitor(c.spec, c.m); got != c.want {
			t.Errorf("matchMonitor(%q, %s) = %v, want %v", c.spec, c.m.device, got, c.want)
		}
	}
}

func TestMatchAnyMonitor(t *testing.T) {
	d2 := monInfo{device: `\\.\DISPLAY2`}
	if !matchAnyMonitor([]string{"primary", "DISPLAY2"}, d2) {
		t.Error("expected a match on DISPLAY2")
	}
	if matchAnyMonitor([]string{"primary"}, d2) || matchAnyMonitor(nil, d2) {
		t.Error("expected no match")
	}
}

// TestListMonitors runs against the real display configuration, so it only
// checks basic sanity (and logs the list with -v).
func TestListMonitors(t *testing.T) {
	list := listMonitors(true)
	if len(list) == 0 {
		t.Skip("no monitors reported (headless session)")
	}
	t.Log(describeMonitors(list))
	primaries := 0
	for _, m := range list {
		if m.handle == 0 || m.device == "" {
			t.Errorf("incomplete monitor: %+v", m)
		}
		if m.primary {
			primaries++
		}
	}
	if primaries != 1 {
		t.Errorf("want exactly one primary monitor, got %d", primaries)
	}
}
