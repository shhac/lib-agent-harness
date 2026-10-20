package textbound

import "bytes"

// Prefix is an io.Writer that keeps the first Limit bytes written to it and
// counts every byte, so a caller can tell output was cut rather than read a
// shortened copy as the whole. Write always accepts everything it is given,
// so a process copying into it never fails with a short write.
type Prefix struct {
	Limit int
	buf   bytes.Buffer
	total int
}

func (p *Prefix) Write(b []byte) (int, error) {
	p.total += len(b)
	if remaining := p.Limit - p.buf.Len(); remaining > 0 {
		_, _ = p.buf.Write(b[:min(len(b), remaining)])
	}
	return len(b), nil
}

// Bytes is the kept prefix.
func (p *Prefix) Bytes() []byte { return p.buf.Bytes() }

// Exceeded reports whether more was written than Limit keeps.
func (p *Prefix) Exceeded() bool { return p.total > p.Limit }
