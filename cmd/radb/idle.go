package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// A component is a part of radb that can say whether anyone is using it.
type component interface {
	Busy() bool
	LastActivity() time.Time
}

// idleWatch stops radb once it has stopped being of use: either nothing has
// connected for a while, or there has been no device for it to reach.
//
// It exists because the far end cannot tell: an ssh tunnel to a machine whose
// phone was unplugged hours ago looks exactly like a working one until a
// command is run through it. Exiting turns that into an ssh that has gone away,
// which is a state the remote side does understand.
type idleWatch struct {
	// After is how long a condition has to hold before radb gives up. Zero
	// disables the watch entirely.
	After time.Duration
	// Parts are the components whose clients count as use.
	Parts []component
	// Devices reports every device visible right now, in either mode.
	Devices func() []string
	Log     *slog.Logger
	Stop    func(reason string)
}

// Run watches until the deadline passes or ctx is cancelled.
func (w *idleWatch) Run(ctx context.Context) {
	if w.After <= 0 {
		return
	}
	// Check often enough that the reported idle time is roughly the configured
	// one, without waking up pointlessly on a long timeout.
	tick := w.After / 10
	if tick < 250*time.Millisecond {
		tick = 250 * time.Millisecond
	} else if tick > time.Minute {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	now := time.Now()
	// Startup counts as activity, and as having just looked for a device.
	// Otherwise a radb started before the phone is plugged in would be judged
	// on a window it was never awake for.
	used, present := now, now
	var devices string

	for {
		select {
		case <-ctx.Done():
			return
		case now = <-t.C:
		}

		busy := false
		for _, p := range w.Parts {
			if p.Busy() {
				busy = true
			}
			if last := p.LastActivity(); last.After(used) {
				used = last
			}
		}
		if busy {
			used = now
		}

		var devs []string
		if w.Devices != nil {
			devs = w.Devices()
		}
		if len(devs) > 0 {
			present = now
		}
		// Plugging a device in is someone arriving, not idleness. Reset the
		// activity clock too, so radb does not exit seconds after the device
		// it was waiting for turns up.
		if list := strings.Join(devs, ","); list != devices {
			devices, used = list, now
		}

		switch {
		case len(devs) == 0 && now.Sub(present) >= w.After:
			w.Stop("no device has been attached for " + short(now.Sub(present)))
			return
		case !busy && now.Sub(used) >= w.After:
			w.Stop("nothing has used it for " + short(now.Sub(used)))
			return
		}
	}
}

// short renders a span at the precision it deserves: minutes for the timeouts
// anyone actually configures, seconds for the short ones used in testing.
func short(d time.Duration) string {
	if d < 2*time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}
