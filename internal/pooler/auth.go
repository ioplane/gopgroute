package pooler

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// Only the initial method is inspected; pgx verifies the complete SCRAM proof.
type scramConnection struct {
	net.Conn
	mu      sync.Mutex
	checked bool
	pending []byte
	failure error
}

func (c *scramConnection) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked {
		c.checked = true
		prefix := make([]byte, 9)
		_, c.failure = io.ReadFull(c.Conn, prefix[:5])
		if c.failure == nil {
			length := binary.BigEndian.Uint32(prefix[1:5])
			if prefix[0] != 'R' || length < 8 || length > 64*1024+4 {
				c.failure = errors.New("SCRAM first message required")
			}
		}
		if c.failure == nil {
			_, c.failure = io.ReadFull(c.Conn, prefix[5:])
			if c.failure == nil && binary.BigEndian.Uint32(prefix[5:]) != 10 {
				c.failure = errors.New("SCRAM first method required")
			}
		}
		c.pending = prefix
	}
	if c.failure != nil {
		return 0, c.failure
	}
	if len(c.pending) > 0 {
		n := copy(dst, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(dst)
}
