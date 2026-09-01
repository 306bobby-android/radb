package fastboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// The four response tags a bootloader can put at the head of a packet, plus
// TEXT, which newer bootloaders use for continuation lines of INFO output.
const (
	respOKAY = "OKAY"
	respFAIL = "FAIL"
	respDATA = "DATA"
	respINFO = "INFO"
	respTEXT = "TEXT"
)

// respMax is the most a bootloader puts in one response packet: AOSP's driver
// reads replies into a fixed 64 byte buffer (FB_RESPONSE_SZ) on every transport.
const respMax = 64

// cmdMax bounds what we will accept as a command, so a desynchronised stream
// fails loudly instead of trying to allocate a bogus length. Real commands are
// capped at 64 bytes by the host tool; the slack is for oem passthrough.
const cmdMax = 4096

// dataChunk is how much of a data phase we try to move per transfer.
const dataChunk = 1 << 20

// usbPort is the bootloader end of a session. *Device implements it; tests
// substitute a scripted bootloader.
type usbPort interface {
	Send(ctx context.Context, b []byte) error
	Recv(ctx context.Context, b []byte) (int, error)
	InPacketSize() int
}

// Session pumps one fastboot client connection through to the bootloader.
type Session struct {
	dev usbPort
	r   *PacketReader
	w   *PacketWriter
	log *slog.Logger
}

// NewSession wires a client stream to a bootloader.
func NewSession(dev usbPort, r *PacketReader, w *PacketWriter, log *slog.Logger) *Session {
	return &Session{dev: dev, r: r, w: w, log: log}
}

// Serve relays commands until the client disconnects or the link breaks.
func (s *Session) Serve(ctx context.Context) error {
	for {
		n, err := s.r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if n > cmdMax {
			return fmt.Errorf("client announced a %d byte command; the stream is out of sync", n)
		}
		cmd, err := s.r.Full()
		if err != nil {
			return fmt.Errorf("read command: %w", err)
		}
		if err := s.command(ctx, string(cmd)); err != nil {
			return err
		}
	}
}

// command forwards one command and everything the bootloader says in reply,
// including any data phase the reply asks for.
//
// Each command is logged with what the bootloader finally said about it, since
// on the remote side that verdict is all anyone sees -- and for a flash or an
// erase, the timing is the only sign of progress this end has.
func (s *Session) command(ctx context.Context, cmd string) error {
	start := time.Now()
	if err := s.dev.Send(ctx, []byte(cmd)); err != nil {
		s.log.Warn("fastboot", "cmd", clipStr(cmd), "err", err)
		return err
	}
	var moved uint64
	for {
		resp, err := s.readResponse(ctx)
		if err != nil {
			s.log.Warn("fastboot", "cmd", clipStr(cmd), "err", err)
			return err
		}
		if err := s.w.Send(resp); err != nil {
			return err
		}

		var tag string
		if len(resp) >= 4 {
			tag = string(resp[:4])
		}
		switch tag {
		case respINFO, respTEXT:
			// Progress chatter: the bootloader has more to say.
			s.log.Debug("fastboot info", "cmd", clipStr(cmd), "text", clipStr(string(resp[4:])))
			continue
		case respDATA:
			size, err := parseDataSize(resp)
			if err != nil {
				return err
			}
			if err := s.dataPhase(ctx, cmd, size); err != nil {
				s.log.Warn("fastboot", "cmd", clipStr(cmd), "data", size, "err", err)
				return err
			}
			moved += size
			// A data phase is always followed by a terminal response.
			continue
		case respOKAY, respFAIL:
			args := []any{"cmd", clipStr(cmd), "result", tag, "took", time.Since(start).Round(time.Millisecond)}
			if msg := strings.TrimSpace(string(resp[4:])); msg != "" {
				args = append(args, "said", clipStr(msg))
			}
			if moved > 0 {
				args = append(args, "bytes", moved)
			}
			s.log.Info("fastboot", args...)
			return nil
		default:
			return fmt.Errorf("bootloader answered %s, which is not a fastboot response", clip(resp))
		}
	}
}

// readResponse reads one reply packet from the bootloader.
func (s *Session) readResponse(ctx context.Context) ([]byte, error) {
	// Size the buffer to the endpoint's max packet so a chatty bootloader can
	// never overflow the transfer, then keep only what it actually sent.
	size := s.dev.InPacketSize()
	if size < respMax {
		size = respMax
	}
	buf := make([]byte, size)
	n, err := s.dev.Recv(ctx, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// parseDataSize reads the hex byte count out of a DATA%08x response.
func parseDataSize(resp []byte) (uint64, error) {
	if len(resp) < 12 {
		return 0, fmt.Errorf("truncated DATA response %s", clip(resp))
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(resp[4:12])), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable size in DATA response %s: %w", clip(resp), err)
	}
	return n, nil
}

// dataPhase moves the payload the bootloader just asked for. Which way it flows
// is decided by the command, not the response: DATA means "send it now" after
// download:, and "here it comes" after upload or fetch:.
func (s *Session) dataPhase(ctx context.Context, cmd string, size uint64) error {
	switch {
	case strings.HasPrefix(cmd, "download:"):
		return s.hostToDevice(ctx, size)
	case cmd == "upload" || strings.HasPrefix(cmd, "fetch:"):
		return s.deviceToHost(ctx, size)
	default:
		return fmt.Errorf("bootloader asked for a %d byte data phase after %q, "+
			"but that command has no defined data direction", size, cmd)
	}
}

// hostToDevice streams size bytes of client packets into the bulk OUT endpoint.
// The host is free to split the payload across packets, so keep pulling packets
// until the byte count is met rather than assuming one packet holds it all.
func (s *Session) hostToDevice(ctx context.Context, size uint64) error {
	buf := make([]byte, dataChunk)
	var moved uint64
	for moved < size {
		if s.r.Left() == 0 {
			if _, err := s.r.Next(); err != nil {
				return fmt.Errorf("read download payload after %d of %d bytes: %w", moved, size, err)
			}
		}
		want := uint64(len(buf))
		if rem := size - moved; rem < want {
			want = rem
		}
		n, err := s.r.Read(buf[:want])
		if n > 0 {
			if err := s.dev.Send(ctx, buf[:n]); err != nil {
				return err
			}
			moved += uint64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read download payload after %d of %d bytes: %w", moved, size, err)
		}
	}
	return nil
}

// deviceToHost streams size bytes from the bulk IN endpoint to the client as a
// single packet. See PacketWriter.Begin for why it cannot be split.
func (s *Session) deviceToHost(ctx context.Context, size uint64) error {
	if err := s.w.Begin(size); err != nil {
		return err
	}
	// Read whole USB packets so a trailing partial packet cannot overflow.
	pkt := s.dev.InPacketSize()
	chunk := dataChunk / pkt * pkt
	if chunk == 0 {
		chunk = pkt
	}
	buf := make([]byte, chunk)

	var moved uint64
	for moved < size {
		n, err := s.dev.Recv(ctx, buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("bootloader went quiet %d bytes into a %d byte upload", moved, size)
		}
		// A bootloader that overshoots would desynchronise the stream; keep
		// only what the DATA header promised.
		if uint64(n) > size-moved {
			n = int(size - moved)
		}
		if err := s.w.Body(buf[:n]); err != nil {
			return err
		}
		moved += uint64(n)
	}
	return s.w.Flush()
}

// clip renders a response for an error message without dumping binary noise.
func clip(b []byte) string {
	const limit = 32
	if len(b) > limit {
		return strconv.Quote(string(b[:limit])) + "..."
	}
	return strconv.Quote(string(b))
}

// clipStr keeps a log line to one line, whatever the bootloader or an oem
// passthrough puts in it.
func clipStr(s string) string {
	const limit = 120
	s = strings.TrimRight(s, "\x00")
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
}
