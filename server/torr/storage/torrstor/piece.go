package torrstor

import (
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"server/settings"
)

type Piece struct {
	storage.PieceImpl `json:"-"`

	Id   int   `json:"-"`
	Size int64 `json:"size"`

	Complete bool  `json:"complete"`
	Accessed int64 `json:"accessed"`

	statMu sync.RWMutex

	mPiece *MemPiece  `json:"-"`
	dPiece *DiskPiece `json:"-"`

	cache *Cache `json:"-"`
}

func NewPiece(id int, cache *Cache) *Piece {
	p := &Piece{
		Id:    id,
		cache: cache,
	}

	if !settings.BTsets.UseDisk {
		p.mPiece = NewMemPiece(p)
	} else {
		p.dPiece = NewDiskPiece(p)
	}
	return p
}

// WriteAt, ReadAt and Release go to the backend the piece was created with. Checking the
// live UseDisk setting instead crashed the server when the setting was saved while a torrent
// was active: SetSettings switches BTsets first and drops torrents after, and in between the
// engine keeps writing and reading the old pieces, whose other backend is nil.
func (p *Piece) WriteAt(b []byte, off int64) (n int, err error) {
	if p.mPiece != nil {
		return p.mPiece.WriteAt(b, off)
	}
	return p.dPiece.WriteAt(b, off)
}

func (p *Piece) ReadAt(b []byte, off int64) (n int, err error) {
	if p.mPiece != nil {
		return p.mPiece.ReadAt(b, off)
	}
	return p.dPiece.ReadAt(b, off)
}

// memCost is what this piece costs the cache budget. In RAM mode that is the allocated buffer
// (a whole piece from the first written byte), not the bytes received; Size stays the logical
// downloaded size used for progress reporting. Disk pieces cost their file size.
func (p *Piece) memCost() int64 {
	if p.mPiece != nil {
		return p.mPiece.allocated()
	}
	return p.getSize()
}

func (p *Piece) snapshot() (size int64, complete bool, accessed int64) {
	p.statMu.RLock()
	size = p.Size
	complete = p.Complete
	accessed = p.Accessed
	p.statMu.RUnlock()
	return
}

func (p *Piece) getSize() int64 {
	p.statMu.RLock()
	size := p.Size
	p.statMu.RUnlock()
	return size
}

func (p *Piece) isComplete() bool {
	p.statMu.RLock()
	complete := p.Complete
	p.statMu.RUnlock()
	return complete
}

func (p *Piece) addSize(delta int64, maxLen int64, now int64) {
	p.statMu.Lock()
	p.Size += delta
	if p.Size > maxLen {
		p.Size = maxLen
	}
	p.Accessed = now
	p.statMu.Unlock()
}

func (p *Piece) touch(now int64) int64 {
	p.statMu.Lock()
	p.Accessed = now
	size := p.Size
	p.statMu.Unlock()
	return size
}

func (p *Piece) resetStats() {
	p.statMu.Lock()
	p.Size = 0
	p.Complete = false
	p.statMu.Unlock()
}

func (p *Piece) MarkComplete() error {
	p.statMu.Lock()
	p.Complete = true
	p.statMu.Unlock()
	return nil
}

func (p *Piece) MarkNotComplete() error {
	p.statMu.Lock()
	p.Complete = false
	p.statMu.Unlock()
	return nil
}

func (p *Piece) Completion() storage.Completion {
	p.statMu.RLock()
	complete := p.Complete
	p.statMu.RUnlock()
	return storage.Completion{
		Complete: complete,
		Ok:       true,
	}
}

func (p *Piece) Release() {
	if p.mPiece != nil {
		p.mPiece.Release()
	} else {
		p.dPiece.Release()
	}
	if tor := p.cache.getTorrent(); tor != nil {
		tor.Piece(p.Id).SetPriority(torrent.PiecePriorityNone)
		tor.Piece(p.Id).UpdateCompletion()
	}
}
