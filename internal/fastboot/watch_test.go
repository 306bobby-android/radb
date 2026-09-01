package fastboot

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// bus is a scripted USB bus: each poll takes the next reading.
type bus struct {
	readings [][]Info
	errs     []error
	polls    int
}

func (b *bus) next() ([]Info, error) {
	i := b.polls
	b.polls++
	if i < len(b.errs) && b.errs[i] != nil {
		return nil, b.errs[i]
	}
	if i >= len(b.readings) {
		return nil, errors.New("bus ran out of readings")
	}
	return b.readings[i], nil
}

func watcher(readings ...[]Info) (*Watcher, *bytes.Buffer, *bus) {
	var out bytes.Buffer
	b := &bus{readings: readings}
	return &Watcher{Log: slog.New(slog.NewTextHandler(&out, nil)), list: b.next}, &out, b
}

// A device going into its bootloader vanishes from adb entirely, so this poll
// is the only notice anyone gets that it happened.
func TestBootloaderComingAndGoing(t *testing.T) {
	w, out, _ := watcher(
		nil,
		[]Info{{Serial: "0A021FDD4005CG", Path: "1-2"}},
		nil,
	)
	w.poll()
	w.poll()
	w.poll()

	log := out.String()
	for _, want := range []string{"bootloader connected", "bootloader disconnected", "0A021FDD4005CG"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not mention %q:\n%s", want, log)
		}
	}
}

// One already plugged in at startup is present, not newly connected.
func TestFirstReadingIsReportedAsPresent(t *testing.T) {
	w, out, _ := watcher([]Info{{Serial: "SER", Path: "1-2"}})
	w.poll()
	if log := out.String(); !strings.Contains(log, "bootloader present") {
		t.Errorf("log = %s", log)
	}
	if got := w.Present(); len(got) != 1 || got[0] != "SER" {
		t.Errorf("Present() = %v", got)
	}
}

// A failed enumeration says nothing about what is plugged in. Reporting it as
// an unplug would hand the idle watch a reason to give up on a live device.
func TestAFailedPollHoldsTheLastReading(t *testing.T) {
	w, out, b := watcher([]Info{{Serial: "SER", Path: "1-2"}}, nil)
	b.errs = []error{nil, errors.New("libusb sulked")}
	w.poll()
	w.poll()

	if got := w.Present(); len(got) != 1 || got[0] != "SER" {
		t.Errorf("Present() = %v, want the last good reading", got)
	}
	if log := out.String(); strings.Contains(log, "disconnected") {
		t.Errorf("a failed poll was reported as an unplug:\n%s", log)
	}
}

// Enumerating USB underneath a session that has the interface claimed is worth
// nobody's trouble.
func TestPollIsSkippedDuringAFlash(t *testing.T) {
	w, _, b := watcher([]Info{{Serial: "SER"}})
	w.Skip = func() bool { return true }
	w.poll()
	if b.polls != 0 {
		t.Errorf("polled %d times during a flash", b.polls)
	}
}
