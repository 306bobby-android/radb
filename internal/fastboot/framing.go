// Package fastboot implements the device side of the fastboot-over-TCP
// protocol and relays it to a bootloader attached over USB.
//
// The wire format is the one documented in AOSP fastboot/README.md: a four byte
// handshake in each direction ("FB" followed by a two digit, base ten version),
// then a stream of packets, each an unsigned eight byte big-endian length
// followed by that many bytes of raw fastboot packet.
package fastboot

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// ProtocolVersion is the fastboot-over-TCP version we advertise. Stock fastboot
// hangs up unless the device's version is at least as high as its own, so this
// tracks the version the host tool speaks rather than the newest one we could.
const ProtocolVersion = 1

// headerLen is the size of the big-endian length prefix carried by every packet.
const headerLen = 8

// Handshake exchanges the four byte version handshake and reports the version
// the peer advertised. Both sides write before either reads, so the exchange
// cannot deadlock against a peer that does the same.
func Handshake(rw io.ReadWriter) (int, error) {
	if _, err := io.WriteString(rw, fmt.Sprintf("FB%02d", ProtocolVersion)); err != nil {
		return 0, fmt.Errorf("write handshake: %w", err)
	}
	var buf [4]byte
	if _, err := io.ReadFull(rw, buf[:]); err != nil {
		return 0, fmt.Errorf("read handshake: %w", err)
	}
	if buf[0] != 'F' || buf[1] != 'B' {
		return 0, fmt.Errorf("peer sent %q, which is not a fastboot handshake", buf[:])
	}
	var peer int
	if _, err := fmt.Sscanf(string(buf[2:]), "%d", &peer); err != nil {
		return 0, fmt.Errorf("peer sent unparseable handshake version %q", buf[2:])
	}
	return peer, nil
}

// PacketReader reads length-prefixed packets from the client. A packet body can
// be consumed in pieces, which matters because the whole download data phase
// arrives as one packet that may be hundreds of megabytes.
type PacketReader struct {
	r    io.Reader
	left uint64
}

// NewPacketReader wraps r. Callers should hand it a buffered reader.
func NewPacketReader(r io.Reader) *PacketReader { return &PacketReader{r: r} }

// Next reads the header of the following packet and returns its length. The
// previous packet must already be fully consumed.
func (p *PacketReader) Next() (uint64, error) {
	if p.left != 0 {
		return 0, fmt.Errorf("packet reader: %d bytes of the previous packet were never read", p.left)
	}
	var hdr [headerLen]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return 0, err
	}
	p.left = binary.BigEndian.Uint64(hdr[:])
	return p.left, nil
}

// Left reports how much of the current packet is still unread.
func (p *PacketReader) Left() uint64 { return p.left }

// Read consumes up to len(b) bytes, never crossing the packet boundary, and
// reports io.EOF once the current packet is exhausted.
func (p *PacketReader) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	if uint64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, err := p.r.Read(b)
	p.left -= uint64(n)
	return n, err
}

// Full reads the whole of the current packet.
func (p *PacketReader) Full() ([]byte, error) {
	b := make([]byte, p.left)
	if _, err := io.ReadFull(p, b); err != nil {
		return nil, err
	}
	return b, nil
}

// PacketWriter writes length-prefixed packets back to the client.
type PacketWriter struct{ w *bufio.Writer }

// NewPacketWriter wraps w with enough buffering to keep upload streaming cheap.
func NewPacketWriter(w io.Writer) *PacketWriter {
	return &PacketWriter{w: bufio.NewWriterSize(w, 256<<10)}
}

// Send writes b as one complete packet and flushes it.
func (p *PacketWriter) Send(b []byte) error {
	if err := p.Begin(uint64(len(b))); err != nil {
		return err
	}
	if err := p.Body(b); err != nil {
		return err
	}
	return p.Flush()
}

// Begin opens a packet of exactly n bytes whose body is streamed with Body and
// finished with Flush.
//
// An upload payload has to go out as a single packet. The host reads it in
// chunks of up to 1 MiB, its TcpTransport.Read never reads across a packet
// boundary, and its ReadBuffer treats a read that returns less than it asked
// for as fatal -- so splitting the payload would break fetch and get_staged.
func (p *PacketWriter) Begin(n uint64) error {
	var hdr [headerLen]byte
	binary.BigEndian.PutUint64(hdr[:], n)
	_, err := p.w.Write(hdr[:])
	return err
}

// Body appends bytes to the packet opened by Begin.
func (p *PacketWriter) Body(b []byte) error {
	_, err := p.w.Write(b)
	return err
}

// Flush pushes buffered bytes onto the socket.
func (p *PacketWriter) Flush() error { return p.w.Flush() }
