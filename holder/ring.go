package holder

// Ring keeps the last bytes of a stream. The zero value keeps DefaultRing
// bytes.
type Ring struct {
	limit int
	buf   []byte
}

// NewRing makes a ring of limit bytes. A limit of 0 or less means
// DefaultRing.
func NewRing(limit int) Ring {
	if limit <= 0 {
		limit = DefaultRing
	}
	return Ring{limit: limit}
}

// Write appends p. A full ring moves its last bytes to the start once for
// each limit bytes of input, so the buffer can grow to twice the limit.
func (r *Ring) Write(p []byte) {
	limit := r.size()
	r.buf = append(r.buf, p...)
	if len(r.buf) > 2*limit {
		n := copy(r.buf, r.buf[len(r.buf)-limit:])
		r.buf = r.buf[:n]
	}
}

// Bytes gives the last bytes. The slice is valid until the next Write or
// Reset. A caller that keeps it must copy it.
func (r *Ring) Bytes() []byte {
	return r.buf[max(0, len(r.buf)-r.size()):]
}

// Reset replaces the content with the last bytes of p. The ring keeps p as
// its buffer.
func (r *Ring) Reset(p []byte) {
	r.buf = p[max(0, len(p)-r.size()):]
}

// Len gives the number of bytes that Bytes returns.
func (r *Ring) Len() int {
	return min(len(r.buf), r.size())
}

func (r *Ring) size() int {
	if r.limit <= 0 {
		return DefaultRing
	}
	return r.limit
}
