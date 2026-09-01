package adbproxy

import (
	"io"
	"strconv"
)

// sniffMax bounds what a sniffer will hold onto while waiting for the rest of
// what it is looking for. Nothing we inspect is anywhere near this big; the cap
// is there so a stream that is not what we assumed cannot grow the buffer.
const sniffMax = 64 << 10

// sniffer passes a stream through untouched while showing the leading bytes to
// want, which reports true once it has seen enough. Reading a service out of a
// spliced connection this way keeps the splice honest: the proxy never holds a
// byte back waiting for something that may not come.
type sniffer struct {
	r    io.Reader
	want func([]byte) bool
	buf  []byte
	done bool
}

func (s *sniffer) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 && !s.done {
		s.buf = append(s.buf, b[:n]...)
		if s.want(s.buf) || len(s.buf) >= sniffMax {
			s.done, s.buf = true, nil
		}
	}
	return n, err
}

// sniffFrame reports the first length-prefixed frame in the stream.
func sniffFrame(fn func(string)) func([]byte) bool {
	return func(buf []byte) bool {
		if len(buf) < 4 {
			return false
		}
		n, err := strconv.ParseUint(string(buf[:4]), 16, 32)
		if err != nil {
			return true // not a frame after all; stop guessing
		}
		if uint64(len(buf)) < 4+n {
			return false
		}
		fn(string(buf[4 : 4+n]))
		return true
	}
}

// sniffSkip hands want everything after the first n bytes, for a reply that
// leads with something other than what is being looked for.
func sniffSkip(n int, want func([]byte) bool) func([]byte) bool {
	if n == 0 {
		return want
	}
	return func(buf []byte) bool {
		if len(buf) < n {
			return false
		}
		return want(buf[n:])
	}
}

// sniffStatus reports the first four byte OKAY/FAIL tag in the stream.
func sniffStatus(fn func(string)) func([]byte) bool {
	return func(buf []byte) bool {
		if len(buf) < 4 {
			return false
		}
		fn(string(buf[:4]))
		return true
	}
}
