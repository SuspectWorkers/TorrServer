package torrstor

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"server/log"
	"server/settings"
)

type DiskPiece struct {
	piece *Piece

	name string

	ff       *os.File
	writable bool
	lastIO   atomic.Int64

	mu sync.RWMutex
}

func NewDiskPiece(p *Piece) *DiskPiece {
	name := filepath.Join(settings.BTsets.TorrentsSavePath, p.cache.hash.HexString(), strconv.Itoa(p.Id))
	ff, err := os.Stat(name)
	if err == nil {
		p.Size = ff.Size()
		p.Complete = ff.Size() == p.cache.pieceLength
		p.Accessed = ff.ModTime().Unix()
	}
	return &DiskPiece{piece: p, name: name}
}

func (p *DiskPiece) acquireWriteFileLocked() (*os.File, bool, error) {
	if p.ff != nil && p.writable {
		return p.ff, false, nil
	}
	if p.ff != nil {
		_ = p.ff.Close()
		p.ff = nil
		p.writable = false
	}
	ff, err := os.OpenFile(p.name, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, false, err
	}
	p.ff = ff
	p.writable = true
	return ff, true, nil
}

func (p *DiskPiece) WriteAt(b []byte, off int64) (n int, err error) {
	p.mu.Lock()
	ff, opened, openErr := p.acquireWriteFileLocked()
	if openErr != nil {
		p.mu.Unlock()
		log.TLogln("Error open file:", openErr)
		return 0, openErr
	}
	n, err = ff.WriteAt(b, off)
	p.mu.Unlock()

	now := time.Now().Unix()
	p.lastIO.Store(now)
	if opened && p.piece.cache != nil {
		p.piece.cache.trackOpenDiskPiece(p)
	}

	p.piece.addSize(int64(n), p.piece.cache.pieceLength, now)
	return
}

func (p *DiskPiece) ReadAt(b []byte, off int64) (n int, err error) {
	p.mu.RLock()
	if p.ff != nil {
		n, err = p.ff.ReadAt(b, off)
		p.mu.RUnlock()

		now := time.Now().Unix()
		p.lastIO.Store(now)
		pieceSize := p.piece.touch(now)
		if int64(len(b))+off >= pieceSize && p.piece.cache != nil {
			p.piece.cache.triggerClean()
		}
		return n, err
	}
	p.mu.RUnlock()

	p.mu.Lock()
	opened := false
	if p.ff == nil {
		ff, openErr := os.OpenFile(p.name, os.O_RDONLY, 0o666)
		if os.IsNotExist(openErr) {
			p.mu.Unlock()
			return 0, io.EOF
		}
		if openErr != nil {
			p.mu.Unlock()
			log.TLogln("Error open file:", openErr)
			return 0, openErr
		}
		p.ff = ff
		p.writable = false
		opened = true
	}
	n, err = p.ff.ReadAt(b, off)
	p.mu.Unlock()

	now := time.Now().Unix()
	p.lastIO.Store(now)
	if opened && p.piece.cache != nil {
		p.piece.cache.trackOpenDiskPiece(p)
	}

	pieceSize := p.piece.touch(now)
	if int64(len(b))+off >= pieceSize && p.piece.cache != nil {
		p.piece.cache.triggerClean()
	}
	return n, err
}

func (p *DiskPiece) closeIdleFile(maxIdleSec int64, now int64) bool {
	if !p.mu.TryLock() {
		return false
	}
	defer p.mu.Unlock()
	if p.ff == nil {
		return true
	}
	if maxIdleSec > 0 && now-p.lastIO.Load() < maxIdleSec {
		return false
	}
	_ = p.ff.Close()
	p.ff = nil
	p.writable = false
	return true
}

func (p *DiskPiece) closeFileForce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ff != nil {
		_ = p.ff.Close()
		p.ff = nil
		p.writable = false
	}
}

func (p *DiskPiece) Release() {
	p.mu.Lock()
	if p.ff != nil {
		_ = p.ff.Close()
		p.ff = nil
		p.writable = false
	}
	p.piece.resetStats()
	err := os.Remove(p.name)
	p.mu.Unlock()

	_ = err
	if p.piece.cache != nil {
		p.piece.cache.untrackDiskPiece(p.piece.Id)
	}
}
