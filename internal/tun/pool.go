package tun

import (
	"context"
	"fmt"
)

// BufferPool is a hard-bounded packet buffer pool. Unlike sync.Pool, its
// allocation count cannot grow under load.
type BufferPool struct {
	buffers chan []byte
	size    int
}

func NewBufferPool(size, count int) (*BufferPool, error) {
	if size <= PacketOffset {
		return nil, fmt.Errorf("buffer size %d must exceed packet offset %d", size, PacketOffset)
	}
	if count < 2 || count > 4096 {
		return nil, fmt.Errorf("buffer count %d must be between 2 and 4096", count)
	}
	pool := &BufferPool{buffers: make(chan []byte, count), size: size}
	for range count {
		pool.buffers <- make([]byte, size)
	}
	return pool, nil
}

func (pool *BufferPool) Get(ctx context.Context) ([]byte, error) {
	select {
	case buffer := <-pool.buffers:
		return buffer[:pool.size], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (pool *BufferPool) Put(buffer []byte) {
	if cap(buffer) < pool.size {
		return
	}
	buffer = buffer[:pool.size]
	select {
	case pool.buffers <- buffer:
	default:
		// Duplicate or foreign returns are discarded rather than blocking.
	}
}

func (pool *BufferPool) Capacity() int  { return cap(pool.buffers) }
func (pool *BufferPool) Available() int { return len(pool.buffers) }
