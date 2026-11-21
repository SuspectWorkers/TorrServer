package torrstor

import (
	"io"
	"sync"
	"time"
)

type MemPiece struct {
	piece *Piece

	buffer []byte
	mu     sync.RWMutex
}

func NewMemPiece(p *Piece) *MemPiece {
	return &MemPiece{piece: p}
}

func (p *MemPiece) WriteAt(b []byte, off int64) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.buffer == nil {
		p.piece.cache.triggerClean()
		p.buffer = p.piece.cache.allocBuffer()
	}
	n = copy(p.buffer[off:], b[:])
	p.piece.addSize(int64(n), p.piece.cache.pieceLength, time.Now().Unix())
	return
}

func (p *MemPiece) ReadAt(b []byte, off int64) (n int, err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	size := len(b)
	if size+int(off) > len(p.buffer) {
		size = len(p.buffer) - int(off)
		if size < 0 {
			size = 0
		}
	}
	if len(p.buffer) < int(off) || len(p.buffer) < int(off)+size {
		return 0, io.EOF
	}
	n = copy(b, p.buffer[int(off) : int(off)+size][:])
	pieceSize := p.piece.touch(time.Now().Unix())
	if int64(len(b))+off >= pieceSize {
		p.piece.cache.triggerClean()
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// allocated returns the RAM held by the backing buffer. The whole piece is allocated on the
// first write, so this can be far larger than the number of payload bytes received so far.
func (p *MemPiece) allocated() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return int64(cap(p.buffer))
}

func (p *MemPiece) Release() {
	p.mu.Lock()
	buf := p.buffer
	p.buffer = nil
	p.mu.Unlock()

	if buf != nil && p.piece != nil && p.piece.cache != nil {
		p.piece.cache.freeBuffer(buf)
	}
	p.piece.resetStats()
}
