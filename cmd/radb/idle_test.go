package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePart stands in for the proxy or the bridge.
type fakePart struct {
	mu   sync.Mutex
	busy bool
	last time.Time
}

func (f *fakePart) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakePart) LastActivity() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakePart) set(busy bool) {
	f.mu.Lock()
	f.busy, f.last = busy, time.Now()
	f.mu.Unlock()
}

// watch runs an idleWatch and reports the reason it stopped, or "" if it did
// not stop before the deadline.
func watch(t *testing.T, w *idleWatch, within time.Duration) string {
	t.Helper()
	stopped := make(chan string, 1)
	w.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	w.Stop = func(reason string) { stopped <- reason }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	select {
	case reason := <-stopped:
		return reason
	case <-time.After(within):
		return ""
	}
}

func TestIdleStopsWhenNothingIsUsingIt(t *testing.T) {
	reason := watch(t, &idleWatch{
		After:   300 * time.Millisecond,
		Parts:   []component{&fakePart{}},
		Devices: func() []string { return []string{"SERIAL"} },
	}, 3*time.Second)
	if reason == "" {
		t.Fatal("an idle radb with nothing using it never gave up")
	}
	if want := "nothing has used it"; !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want it to mention %q", reason, want)
	}
}

// A device unplugged for the whole window is the other way radb stops being of
// any use, and the one the remote side has no way of noticing.
func TestIdleStopsWhenNoDeviceIsAttached(t *testing.T) {
	busy := &fakePart{}
	busy.set(true)
	reason := watch(t, &idleWatch{
		After:   300 * time.Millisecond,
		Parts:   []component{busy},
		Devices: func() []string { return nil },
	}, 3*time.Second)
	if want := "no device"; !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want it to mention %q", reason, want)
	}
}

// A connection that is open but silent -- adb shell, a long erase -- is someone
// using the device, not idleness.
func TestAnOpenClientKeepsItAlive(t *testing.T) {
	part := &fakePart{}
	part.set(true)
	reason := watch(t, &idleWatch{
		After:   300 * time.Millisecond,
		Parts:   []component{part},
		Devices: func() []string { return []string{"SERIAL"} },
	}, time.Second)
	if reason != "" {
		t.Errorf("stopped underneath a connected client: %s", reason)
	}
}

// Plugging a device in is someone arriving. Counting it as activity is what
// stops radb exiting seconds after the device it was waiting for turns up.
func TestPluggingADeviceInResetsTheClock(t *testing.T) {
	var mu sync.Mutex
	devs := []string{}
	go func() {
		time.Sleep(250 * time.Millisecond)
		mu.Lock()
		devs = []string{"SERIAL"}
		mu.Unlock()
	}()
	reason := watch(t, &idleWatch{
		After: 400 * time.Millisecond,
		Parts: []component{&fakePart{}},
		Devices: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return devs
		},
	}, 600*time.Millisecond)
	if reason != "" {
		t.Errorf("stopped just as a device appeared: %s", reason)
	}
}

func TestZeroIdleNeverGivesUp(t *testing.T) {
	reason := watch(t, &idleWatch{
		After:   0,
		Parts:   []component{&fakePart{}},
		Devices: func() []string { return nil },
	}, 500*time.Millisecond)
	if reason != "" {
		t.Errorf("-idle=0 stopped anyway: %s", reason)
	}
}
