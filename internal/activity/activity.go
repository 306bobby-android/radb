// Package activity tracks when a component last did something for a client, so
// that an idle radb can decide it is no longer wanted and exit.
package activity

import (
	"sync"
	"time"
)

// Tracker counts the clients a component is currently serving and remembers
// when the last one finished. A component is idle only when both say so: a
// silent but open connection -- an adb shell nobody is typing into, a fastboot
// waiting on an erase -- is still someone using the device.
type Tracker struct {
	mu   sync.Mutex
	busy int
	last time.Time
}

// Begin marks a client as being served and returns the func that ends it.
func (t *Tracker) Begin() func() {
	t.mu.Lock()
	t.busy++
	t.last = time.Now()
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.busy--
			t.last = time.Now()
			t.mu.Unlock()
		})
	}
}

// Touch records activity that is not tied to a connection's lifetime.
func (t *Tracker) Touch() {
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

// Busy reports whether any client is currently being served.
func (t *Tracker) Busy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.busy > 0
}

// Last is when a client was most recently served. It is the zero time until
// the first one arrives, which callers read as "nothing has happened yet".
func (t *Tracker) Last() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}
