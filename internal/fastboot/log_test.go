package fastboot

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// runLogged drives a session the way run does, but keeps the log.
func runLogged(t *testing.T, boot *fakeBoot, client []byte) string {
	t.Helper()
	var wire, log bytes.Buffer
	s := NewSession(boot,
		NewPacketReader(bytes.NewReader(client)),
		NewPacketWriter(&wire),
		slog.New(slog.NewTextHandler(&log, nil)))
	s.Serve(context.Background())
	return log.String()
}

// Over the bridge the bootloader's verdict is all the remote side sees, so this
// end has to be able to say what was asked and what came back.
func TestCommandsAndTheirResultsAreLogged(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("OKAYredfin")}}
	log := runLogged(t, boot, pkt([]byte("getvar:product")))
	for _, want := range []string{"getvar:product", "result=OKAY", "said=redfin"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not mention %q:\n%s", want, log)
		}
	}
}

func TestAFailureIsLoggedWithTheBootloadersReason(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("FAILdevice is locked")}}
	log := runLogged(t, boot, pkt([]byte("flash:boot_a")))
	if !strings.Contains(log, "result=FAIL") || !strings.Contains(log, "device is locked") {
		t.Errorf("log does not carry the refusal:\n%s", log)
	}
}

// Progress chatter is what -v is for; it must not drown the result at the
// default level.
func TestInfoLinesAreOnlyLoggedWhenAskedFor(t *testing.T) {
	replies := [][]byte{[]byte("INFOerasing"), []byte("OKAY")}
	boot := &fakeBoot{replies: replies}
	if log := runLogged(t, boot, pkt([]byte("erase:cache"))); strings.Contains(log, "erasing") {
		t.Errorf("bootloader chatter reached the default log:\n%s", log)
	}

	var wire, out bytes.Buffer
	boot = &fakeBoot{replies: [][]byte{[]byte("INFOerasing"), []byte("OKAY")}}
	s := NewSession(boot,
		NewPacketReader(bytes.NewReader(pkt([]byte("erase:cache")))),
		NewPacketWriter(&wire),
		slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s.Serve(context.Background())
	if !strings.Contains(out.String(), "erasing") {
		t.Errorf("-v did not show the chatter:\n%s", out.String())
	}
}
