package main

import "testing"

func TestTrayStatusLifecycle(t *testing.T) {
	for _, test := range []struct {
		name        string
		cancel      bool
		fail        bool
		usesCopilot bool
	}{
		{name: "copilot", usesCopilot: true},
		{name: "provider only remains running without github auth"},
		{name: "failed startup", fail: true},
		{name: "canceled startup", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var lifecycle menubarProxyLifecycle
			checkTrayStatus(t, &lifecycle, trayStopped)
			_, generation, ok := lifecycle.beginStartup(t.Context())
			if !ok {
				t.Fatal("startup rejected")
			}
			checkTrayStatus(t, &lifecycle, trayStarting)
			if test.cancel {
				if !lifecycle.cancelStartup(false) {
					t.Fatal("cancel rejected")
				}
				checkTrayStatus(t, &lifecycle, trayStopping)
			}
			var started menubarProxyServer
			if !test.fail {
				started = &fakeMenubarProxyServer{running: true, usesCopilot: test.usesCopilot}
			}
			lifecycle.finishStartup(generation, started)
			want := trayRunning
			if test.fail || test.cancel {
				want = trayStopped
			}
			checkTrayStatus(t, &lifecycle, want)
			// repeated session refreshes must not depend on authentication.
			checkTrayStatus(t, &lifecycle, want)
		})
	}
}

func TestTrayStatusWhileServerStops(t *testing.T) {
	current := &fakeMenubarProxyServer{running: true}
	lifecycle := menubarProxyLifecycle{server: current}
	checkTrayStatus(t, &lifecycle, trayRunning)
	if got := lifecycle.beginStop(nil); got != current {
		t.Fatalf("beginStop(nil) = %v, want current server", got)
	}
	checkTrayStatus(t, &lifecycle, trayStopping)
	if _, _, ok := lifecycle.beginStartup(t.Context()); ok {
		t.Fatal("startup accepted before the previous server finished stopping")
	}
	if got := lifecycle.beginStop(current); got != nil {
		t.Fatal("listener exit claimed a server already stopping")
	}
	lifecycle.finishStop()
	checkTrayStatus(t, &lifecycle, trayStopped)
}

func TestTrayStatusAfterUnexpectedListenerExit(t *testing.T) {
	exited := &fakeMenubarProxyServer{}
	lifecycle := menubarProxyLifecycle{server: exited}
	if _, _, ok := lifecycle.beginStartup(t.Context()); ok {
		t.Fatal("startup discarded the exited server before cleanup")
	}
	if got := lifecycle.beginStop(exited); got != exited {
		t.Fatalf("beginStop(exited) = %v, want exited server", got)
	}
	lifecycle.finishStop()
	checkTrayStatus(t, &lifecycle, trayStopped)

	_, generation, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("startup rejected after cleanup")
	}
	lifecycle.finishStartup(generation, &fakeMenubarProxyServer{running: true})
	if got := lifecycle.beginStop(exited); got != nil {
		t.Fatal("stale listener exit claimed the replacement server")
	}
	checkTrayStatus(t, &lifecycle, trayRunning)
}

func TestTrayStatusIgnoresSupersededStartup(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	_, first, _ := lifecycle.beginStartup(t.Context())
	lifecycle.cancelStartup(true)
	lifecycle.finishStartup(first, nil)
	_, second, _ := lifecycle.beginStartup(t.Context())
	lifecycle.finishStartup(first, &fakeMenubarProxyServer{running: true})
	checkTrayStatus(t, &lifecycle, trayStarting)
	lifecycle.finishStartup(second, &fakeMenubarProxyServer{running: true})
	checkTrayStatus(t, &lifecycle, trayRunning)
	lifecycle.finishStartup(first, nil)
	checkTrayStatus(t, &lifecycle, trayRunning)
	lifecycle.shutdown()
	lifecycle.finishStartup(second, &fakeMenubarProxyServer{running: true})
	checkTrayStatus(t, &lifecycle, trayStopped)
}

func TestTrayStatusTooltips(t *testing.T) {
	for _, test := range []struct {
		status trayStatus
		want   string
	}{
		{trayStopped, "Vekil - Stopped"},
		{trayStarting, "Vekil - Starting"},
		{trayStopping, "Vekil - Stopping"},
		{trayRunning, "Vekil - Running on :1337"},
	} {
		t.Run(test.want, func(t *testing.T) {
			if got := test.status.tooltip(); got != test.want {
				t.Fatalf("tooltip = %q, want %q", got, test.want)
			}
		})
	}
}

func checkTrayStatus(t *testing.T, lifecycle *menubarProxyLifecycle, want trayStatus) {
	t.Helper()
	if got := lifecycle.trayStatus(); got != want {
		t.Fatalf("tray status = %v, want %v", got, want)
	}
}
