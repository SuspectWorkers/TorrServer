package torrstor

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"server/log"
	"server/settings"
)

type Reader struct {
	torrent.Reader
	offset    int64
	readahead int64
	file      *torrent.File

	cache    *Cache
	isClosed bool

	// ctx is the lifetime of whoever consumes the data (an HTTP request, ReadFile).
	// Set once by SetContext before the first Read.
	ctx context.Context

	// inFlight counts reads and seeks inside the underlying reader; guarded by mu.
	inFlight int

	///Preload
	lastAccess int64
	isUse      bool
	mu         sync.Mutex
}

func newReader(file *torrent.File, cache *Cache) *Reader {
	r := new(Reader)
	r.file = file
	r.Reader = file.NewReader()

	r.cache = cache
	r.isUse = true
	now := time.Now().Unix()
	r.lastAccess = now
	r.Reader.SetReadahead(0)

	cache.muReaders.Lock()
	cache.readers[r] = struct{}{}
	cache.muReaders.Unlock()

	if cache != nil {
		cache.touchActivity(file, 0, now)
	}
	return r
}

func (r *Reader) Seek(offset int64, whence int) (n int64, err error) {
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return 0, io.EOF
	}
	switch whence {
	case io.SeekStart:
		r.offset = offset
	case io.SeekCurrent:
		r.offset += offset
	case io.SeekEnd:
		r.offset = r.file.Length() + offset
	}
	r.inFlight++
	r.resume()
	r.mu.Unlock()
	defer r.endIO()

	n, err = r.Reader.Seek(offset, whence)
	now := time.Now().Unix()
	r.mu.Lock()
	r.offset = n
	r.lastAccess = now
	r.mu.Unlock()

	if r.cache != nil {
		r.cache.touchActivity(r.file, n, now)
	}
	return
}

func (r *Reader) Read(p []byte) (n int, err error) {
	err = io.EOF
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	if r.file.Torrent() == nil || r.file.Torrent().Info() == nil {
		r.mu.Unlock()
		log.TLogln("Torrent closed and readed")
		return
	}
	r.inFlight++
	r.resume()
	r.mu.Unlock()
	defer r.endIO()

	n, err = r.readBackend(p)
	now := time.Now().Unix()
	r.mu.Lock()
	r.offset += int64(n)
	newOff := r.offset
	r.lastAccess = now
	r.mu.Unlock()

	if r.cache != nil {
		r.cache.touchActivity(r.file, newOff, now)
	}
	return
}

// SetContext ties reads to ctx. anacrolix Read waits with context.Background(): a read
// blocked on a piece no peer has yet only returns when the data arrives or the torrent
// closes, and closing the reader does not wake it. Until then the reader stays registered
// and keeps its piece priorities at the old position, e.g. after the player seeks and
// drops the connection. With a context the read returns ctx.Err() as soon as it is done.
func (r *Reader) SetContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *Reader) readBackend(p []byte) (int, error) {
	if r.ctx != nil {
		return r.Reader.ReadContext(r.ctx, p)
	}
	return r.Reader.Read(p)
}

func (r *Reader) SetReadahead(length int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setReadaheadLocked(length)
}

func (r *Reader) setReadaheadLocked(length int64) {
	if r.cache != nil {
		capLimit := r.cache.GetCapacity()
		if capLimit > 0 && length > capLimit {
			length = capLimit
		}
	}
	if r.readahead == length && r.isUse {
		return
	}
	if r.isUse {
		r.Reader.SetReadahead(length)
	}
	r.readahead = length
}

func (r *Reader) Offset() int64 {
	r.mu.Lock()
	off := r.offset
	r.mu.Unlock()
	return off
}

func (r *Reader) Readahead() int64 {
	r.mu.Lock()
	ra := r.readahead
	r.mu.Unlock()
	return ra
}

func (r *Reader) isInUse() bool {
	r.mu.Lock()
	use := r.isUse
	r.mu.Unlock()
	return use
}

func (r *Reader) lastAccessTime() int64 {
	r.mu.Lock()
	ts := r.lastAccess
	r.mu.Unlock()
	return ts
}

func (r *Reader) Close() {
	// file reader close in gotorrent
	// this struct close in cache
	r.mu.Lock()
	r.isClosed = true
	r.mu.Unlock()
	if r.file != nil && r.file.Torrent() != nil && len(r.file.Torrent().Files()) > 0 {
		r.Reader.Close()
	}
	if r.cache != nil {
		r.cache.triggerClean()
	}
}

func (r *Reader) getPiecesRange() Range {
	startOff, endOff := r.getOffsetRange()
	return Range{r.getPieceNum(startOff), r.getPieceNum(endOff), r.file}
}

func (r *Reader) getReaderPiece() int {
	return r.getPieceNum(r.Offset())
}

func (r *Reader) getReaderRAHPiece() int {
	r.mu.Lock()
	pos := r.offset + r.readahead
	r.mu.Unlock()
	return r.getPieceNum(pos)
}

func (r *Reader) getPieceNum(offset int64) int {
	return int((offset + r.file.Offset()) / r.cache.pieceLength)
}

func (r *Reader) getOffsetRange() (int64, int64) {
	prc := int64(settings.BTsets.ReaderReadAHead)
	off := r.Offset()
	readers := int64(r.getUseReaders())
	if readers == 0 {
		readers = 1
	}

	capLimit := r.cache.GetCapacity()
	beginOffset := off - (capLimit/readers)*(100-prc)/100
	endOffset := off + (capLimit/readers)*prc/100

	if beginOffset < 0 {
		beginOffset = 0
	}

	if endOffset > r.file.Length() {
		endOffset = r.file.Length()
	}
	return beginOffset, endOffset
}

func (r *Reader) checkReader(hasActivePeer bool) {
	totalReaders := r.cache.Readers()
	last := r.lastAccessTime()
	now := time.Now().Unix()

	// Do not park the sole reader during a normal playback pause (1-5 minutes):
	// parking seeks the underlying reader to 0 and clears priorities, causing peers to drop.
	// Only park an idle secondary reader if another reader is actively streaming (90s),
	// or if the reader has been completely idle for 5 minutes (300s).
	if (totalReaders > 1 && hasActivePeer && now > last+90) || now > last+300 {
		r.readerOff()
	} else {
		r.readerOn()
	}
}

// beginIO claims the reader for one read or seek and resumes it if the sweep parked it.
// Registering under mu is what keeps readerOff from parking it until endIO.
func (r *Reader) beginIO() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
	r.resume()
}

func (r *Reader) endIO() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

func (r *Reader) readerOn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resume()
}

// resume must be called with mu held.
func (r *Reader) resume() {
	if !r.isUse {
		if pos, err := r.Reader.Seek(0, io.SeekCurrent); err == nil && pos == 0 {
			r.Reader.Seek(r.offset, io.SeekStart)
		}
		r.isUse = true
		r.Reader.SetReadahead(r.readahead)
	}
}

func (r *Reader) readerOff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// lastAccess is stamped only when a read returns, so a read blocked on the swarm makes a
	// busy reader look idle; seeking the anacrolix reader under it moves that read.
	if r.inFlight > 0 {
		return
	}
	if r.isUse {
		r.Reader.SetReadahead(0)
		r.isUse = false
		if r.offset > 0 {
			r.Reader.Seek(0, io.SeekStart)
		}
	}
}

func (r *Reader) getUseReaders() int {
	return r.cache.GetUseReaders()
}
