package manager

import "sync"

// ringBuffer keeps the most recent N bytes of a session's output so that a
// browser attaching late still sees the current screen instead of a blank
// terminal.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []byte
	size  int
	start int // index of the oldest byte when full
	full  bool
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, 0, size), size: size}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	// A write larger than the whole buffer: keep only its tail.
	if n >= r.size {
		r.buf = r.buf[:r.size]
		copy(r.buf, p[n-r.size:])
		r.start = 0
		r.full = true
		return n, nil
	}

	for _, b := range p {
		if !r.full {
			r.buf = append(r.buf, b)
			if len(r.buf) == r.size {
				r.full = true
				r.start = 0
			}
			continue
		}
		r.buf[r.start] = b
		r.start = (r.start + 1) % r.size
	}
	return n, nil
}

// Bytes returns the buffered output in chronological order.
func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out
	}
	out := make([]byte, 0, r.size)
	out = append(out, r.buf[r.start:]...)
	out = append(out, r.buf[:r.start]...)
	return out
}
