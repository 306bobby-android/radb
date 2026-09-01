package fastboot

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultWatchInterval is how often the watcher re-enumerates USB. A device
// crossing between Android and its bootloader is gone for tens of seconds
// either way, so there is nothing to gain from looking harder than this.
const DefaultWatchInterval = 5 * time.Second

// Watcher reports bootloaders appearing on and disappearing from USB, which is
// the only notice anyone gets that a device has gone into fastboot mode: adb
// cannot see it at all once it is there.
type Watcher struct {
	// Interval defaults to DefaultWatchInterval.
	Interval time.Duration
	// Skip is consulted before every poll. Enumerating USB underneath a
	// session that has the interface claimed is worth nobody's trouble, so a
	// true answer means "leave it alone and keep the last reading".
	Skip func() bool
	Log  *slog.Logger

	// list defaults to List; tests replace it with a scripted USB bus.
	list func() ([]Info, error)

	mu      sync.Mutex
	present map[string]string // serial -> usb path
	seen    bool
}

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	w.poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.poll()
		}
	}
}

func (w *Watcher) poll() {
	if w.Skip != nil && w.Skip() {
		return
	}
	enumerate := w.list
	if enumerate == nil {
		enumerate = List
	}
	list, err := enumerate()
	if err != nil {
		// A failed enumeration says nothing about what is plugged in, so hold
		// the last reading rather than reporting everything as unplugged.
		w.Log.Debug("could not enumerate usb", "err", err)
		return
	}
	now := make(map[string]string, len(list))
	for _, d := range list {
		now[d.Serial] = d.Path
	}

	w.mu.Lock()
	was, first := w.present, !w.seen
	w.present, w.seen = now, true
	w.mu.Unlock()

	for serial, path := range now {
		if _, had := was[serial]; !had {
			if first {
				w.Log.Info("bootloader present", "serial", serial, "usb", path)
			} else {
				w.Log.Info("bootloader connected", "serial", serial, "usb", path)
			}
		}
	}
	for serial, path := range was {
		if _, still := now[serial]; !still {
			w.Log.Info("bootloader disconnected", "serial", serial, "usb", path)
		}
	}
}

// Present lists the serials seen by the last successful poll.
func (w *Watcher) Present() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.present))
	for s := range w.present {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
