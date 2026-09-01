package adbproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// svcTrack streams the device list: one frame per change, each holding the
// whole list. It is how adb's own clients watch for devices, and the only way
// to hear about one without polling.
const svcTrack = "host:track-devices"

// Track follows the upstream server's device list and logs what comes and goes,
// until ctx is cancelled. The connection is expected to drop -- the adb server
// can be restarted underneath us -- so it is rebuilt rather than given up on.
func (p *Proxy) Track(ctx context.Context) {
	const retry = 2 * time.Second
	for ctx.Err() == nil {
		if err := p.track(ctx); err != nil && ctx.Err() == nil {
			p.log().Debug("device tracking stopped", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func (p *Proxy) track(ctx context.Context) error {
	up, err := net.DialTimeout("tcp", p.Upstream, 5*time.Second)
	if err != nil {
		return err
	}
	defer up.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		up.Close()
	}()

	if err := writeFrame(up, []byte(svcTrack)); err != nil {
		return err
	}
	var status [4]byte
	if _, err := io.ReadFull(up, status[:]); err != nil {
		return err
	}
	if string(status[:]) != "OKAY" {
		return fmt.Errorf("adb server refused %s", svcTrack)
	}
	for {
		payload, err := readFrame(up)
		if err != nil {
			return err
		}
		p.update(parseDeviceList(string(payload)))
	}
}

// parseDeviceList turns the two column list into serial -> state.
func parseDeviceList(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		out[f[0]] = f[1]
	}
	return out
}

// update logs the difference against the previous list and stores the new one.
func (p *Proxy) update(now map[string]string) {
	p.mu.Lock()
	was, first := p.devs, !p.tracked
	p.devs, p.tracked = now, true
	p.mu.Unlock()

	for _, serial := range sortedKeys(now) {
		switch old, had := was[serial]; {
		case !had && first:
			p.log().Info("adb device present", "serial", serial, "state", now[serial])
		case !had:
			p.log().Info("adb device connected", "serial", serial, "state", now[serial])
		case old != now[serial]:
			p.log().Info("adb device changed state", "serial", serial, "from", old, "to", now[serial])
		}
	}
	for _, serial := range sortedKeys(was) {
		if _, still := now[serial]; !still {
			p.log().Info("adb device disconnected", "serial", serial, "was", was[serial])
		}
	}
}

// Devices lists the serials adb can currently see, in any state.
func (p *Proxy) Devices() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return sortedKeys(p.devs)
}
