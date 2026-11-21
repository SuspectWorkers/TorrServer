package torrstor

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"

	"server/log"
	"server/settings"
	"server/torr/storage/state"
	"server/torr/utils"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	defaultPauseWarmSeconds = 180
	maxOpenDiskFiles        = 32
	diskFileIdleSeconds     = 15
)

type Cache struct {
	storage.TorrentImpl
	storage *Storage

	// filled is the eviction input: memory the pieces occupy (allocated buffers in RAM mode),
	// recomputed by getRemPieces. It is not the downloaded byte count (see GetState).
	capacity int64
	muCap    sync.RWMutex
	filled   int64
	hash     metainfo.Hash

	pieceLength int64
	pieceCount  int

	// pieces content is immutable after Init; muPieces guards the map
	// reference itself (nilled in Close), so holders of a snapshot may
	// safely iterate it without the lock
	pieces   map[int]*Piece
	muPieces sync.RWMutex

	readers   map[*Reader]struct{}
	muReaders sync.RWMutex

	isRemove   atomic.Bool
	isClosed   atomic.Bool
	preloading atomic.Bool
	muRemove   sync.Mutex
	// muPrio serializes priority updates so concurrent callers cannot wipe
	// freshly assigned piece priorities.
	muPrio  sync.Mutex
	torrent atomic.Pointer[torrent.Torrent]

	bufPool   atomic.Pointer[sync.Pool]
	cleanCh   chan struct{}
	stopCh    chan struct{}
	loopDone  chan struct{}
	closeOnce sync.Once

	muAct           sync.Mutex
	lastFile        *torrent.File
	lastOffset      int64
	lastActive      int64
	hadWarmPriority bool

	muDiskPool sync.Mutex
	openDisk   map[int]*DiskPiece
}

func NewCache(capacity int64, storage *Storage) *Cache {
	ret := &Cache{
		capacity: capacity,
		filled:   0,
		pieces:   make(map[int]*Piece),
		storage:  storage,
		readers:  make(map[*Reader]struct{}),
		openDisk: make(map[int]*DiskPiece),
	}

	return ret
}

func (c *Cache) Init(info *metainfo.Info, hash metainfo.Hash) {
	log.TLogln("Create cache for:", info.Name, hash.HexString())
	c.muCap.Lock()
	if c.capacity == 0 {
		c.capacity = info.PieceLength * 4
	}
	c.muCap.Unlock()

	c.pieceLength = info.PieceLength
	c.pieceCount = info.NumPieces()
	c.hash = hash

	pieceLen := info.PieceLength
	c.bufPool.Store(&sync.Pool{
		New: func() any {
			return make([]byte, pieceLen)
		},
	})
	c.cleanCh = make(chan struct{}, 1)
	c.stopCh = make(chan struct{})
	c.loopDone = make(chan struct{})

	if settings.BTsets.UseDisk {
		name := filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString())
		err := os.MkdirAll(name, 0o777)
		if err != nil {
			log.TLogln("Error create dir:", err)
		}
	}

	for i := 0; i < c.pieceCount; i++ {
		c.pieces[i] = NewPiece(i, c)
	}

	go c.cacheLoop()
}

// cacheLoop serializes background eviction and periodic priority re-arming.
//
// Piece priorities are normally updated by I/O-triggered cleanups, but if
// priorities ever end up cleared while a reader (or warm pause window) needs
// data, no piece I/O happens; the 5-second ticker re-arms them and cleans up
// warm priorities once the pause grace window expires.
func (c *Cache) cacheLoop() {
	defer close(c.loopDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-c.cleanCh:
			if c.isClosed.Load() {
				return
			}
			c.cleanPieces()
		case <-ticker.C:
			if c.isClosed.Load() {
				return
			}
			c.trimIdleDiskFiles(diskFileIdleSeconds)
			if c.getTorrent() == nil {
				continue
			}
			if c.GetUseReaders() > 0 || c.hasWarmOrExpiredPriority() {
				c.cleanPieces()
			}
		}
	}
}

func (c *Cache) triggerClean() {
	if c == nil || c.isClosed.Load() {
		return
	}
	if c.cleanCh != nil {
		select {
		case c.cleanCh <- struct{}{}:
		default:
		}
	}
}

func (c *Cache) allocBuffer() []byte {
	if c == nil || c.pieceLength <= 0 {
		return nil
	}
	if pool := c.bufPool.Load(); pool != nil {
		if v := pool.Get(); v != nil {
			if buf, ok := v.([]byte); ok && int64(cap(buf)) >= c.pieceLength {
				buf = buf[:c.pieceLength]
				clear(buf)
				return buf
			}
		}
	}
	return make([]byte, c.pieceLength)
}

func (c *Cache) freeBuffer(buf []byte) {
	if c == nil || c.isClosed.Load() || c.pieceLength <= 0 {
		return
	}
	if pool := c.bufPool.Load(); pool != nil && int64(cap(buf)) >= c.pieceLength {
		pool.Put(buf[:c.pieceLength])
	}
}

func (c *Cache) SetPreloading(v bool) {
	if c == nil {
		return
	}
	c.preloading.Store(v)
}

func (c *Cache) touchActivity(file *torrent.File, offset int64, now int64) {
	if c == nil || file == nil {
		return
	}
	c.muAct.Lock()
	c.lastFile = file
	c.lastOffset = offset
	c.lastActive = now
	c.muAct.Unlock()
}

func (c *Cache) activitySnapshot() (*torrent.File, int64, int64) {
	c.muAct.Lock()
	f, off, act := c.lastFile, c.lastOffset, c.lastActive
	c.muAct.Unlock()
	return f, off, act
}

func (c *Cache) pauseWarmWindowSeconds() int64 {
	if settings.BTsets == nil {
		return defaultPauseWarmSeconds
	}
	if settings.BTsets.PauseTimeout < 0 {
		return 0
	}
	if settings.BTsets.PauseTimeout == 0 {
		return defaultPauseWarmSeconds
	}
	return int64(settings.BTsets.PauseTimeout)
}

func (c *Cache) hasWarmOrExpiredPriority() bool {
	warmSec := c.pauseWarmWindowSeconds()
	c.muAct.Lock()
	has := c.hadWarmPriority || (warmSec > 0 && c.lastFile != nil && time.Now().Unix()-c.lastActive <= warmSec)
	c.muAct.Unlock()
	return has
}

func (c *Cache) trackOpenDiskPiece(dp *DiskPiece) {
	if c == nil || dp == nil || dp.piece == nil {
		return
	}
	var evict []*DiskPiece
	c.muDiskPool.Lock()
	if c.openDisk == nil {
		c.openDisk = make(map[int]*DiskPiece)
	}
	c.openDisk[dp.piece.Id] = dp
	if len(c.openDisk) > maxOpenDiskFiles {
		type candidate struct {
			id   int
			dp   *DiskPiece
			last int64
		}
		list := make([]candidate, 0, len(c.openDisk))
		for id, item := range c.openDisk {
			if id == dp.piece.Id {
				continue
			}
			list = append(list, candidate{id: id, dp: item, last: item.lastIO.Load()})
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].last < list[j].last
		})
		over := len(c.openDisk) - maxOpenDiskFiles
		for i := 0; i < len(list) && over > 0; i++ {
			evict = append(evict, list[i].dp)
			delete(c.openDisk, list[i].id)
			over--
		}
	}
	c.muDiskPool.Unlock()

	for _, item := range evict {
		if !item.closeIdleFile(0, 0) {
			c.muDiskPool.Lock()
			if c.openDisk != nil {
				c.openDisk[item.piece.Id] = item
			}
			c.muDiskPool.Unlock()
		}
	}
}

func (c *Cache) untrackDiskPiece(id int) {
	if c == nil {
		return
	}
	c.muDiskPool.Lock()
	delete(c.openDisk, id)
	c.muDiskPool.Unlock()
}

func (c *Cache) trimIdleDiskFiles(maxIdleSec int64) {
	if c == nil {
		return
	}
	now := time.Now().Unix()
	var list []*DiskPiece
	c.muDiskPool.Lock()
	for _, item := range c.openDisk {
		if now-item.lastIO.Load() >= maxIdleSec {
			list = append(list, item)
		}
	}
	c.muDiskPool.Unlock()

	for _, item := range list {
		if item.closeIdleFile(maxIdleSec, now) {
			c.untrackDiskPiece(item.piece.Id)
		}
	}
}

func (c *Cache) closeAllDiskFiles() {
	if c == nil {
		return
	}
	c.muDiskPool.Lock()
	list := make([]*DiskPiece, 0, len(c.openDisk))
	for _, item := range c.openDisk {
		list = append(list, item)
	}
	c.openDisk = make(map[int]*DiskPiece)
	c.muDiskPool.Unlock()

	for _, item := range list {
		item.closeFileForce()
	}
}

func (c *Cache) openDiskFilesCount() int {
	c.muDiskPool.Lock()
	n := len(c.openDisk)
	c.muDiskPool.Unlock()
	return n
}

func (c *Cache) SetTorrent(torr *torrent.Torrent) {
	if c == nil {
		return
	}
	c.torrent.Store(torr)
}

func (c *Cache) getTorrent() *torrent.Torrent {
	if c == nil {
		return nil
	}
	return c.torrent.Load()
}

func (c *Cache) getPieces() map[int]*Piece {
	c.muPieces.RLock()
	defer c.muPieces.RUnlock()
	return c.pieces
}

func (c *Cache) readersSnapshot() []*Reader {
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	list := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		list = append(list, r)
	}
	return list
}

func (c *Cache) Piece(m metainfo.Piece) storage.PieceImpl {
	if val, ok := c.getPieces()[m.Index()]; ok {
		return val
	}
	return &PieceFake{}
}

func (c *Cache) Close() error {
	if tor := c.getTorrent(); tor != nil {
		log.TLogln("Close cache for:", tor.Name(), c.hash)
	} else {
		log.TLogln("Close cache for:", c.hash)
	}
	c.isClosed.Store(true)
	c.closeOnce.Do(func() {
		if c.stopCh != nil {
			close(c.stopCh)
		}
	})

	if c.storage != nil {
		c.storage.removeCache(c.hash)
	}

	c.closeAllDiskFiles()

	if settings.BTsets != nil && settings.BTsets.RemoveCacheOnDrop {
		name := filepath.Join(settings.BTsets.TorrentsSavePath, c.hash.HexString())
		if name != "" && name != "/" {
			for _, v := range c.getPieces() {
				if v.dPiece != nil {
					os.Remove(v.dPiece.name)
				}
			}
			os.Remove(name)
		}
	}

	c.muReaders.Lock()
	c.readers = nil
	c.muReaders.Unlock()

	c.muPieces.Lock()
	c.pieces = nil
	c.muPieces.Unlock()

	c.bufPool.Store(nil)
	utils.FreeOSMemGC()
	return nil
}

func (c *Cache) removePiece(piece *Piece) {
	if !c.isClosed.Load() {
		piece.Release()
	}
}

func (c *Cache) AdjustRA(readahead int64) {
	if c == nil {
		return
	}
	if settings.BTsets != nil && settings.BTsets.CacheSize == 0 {
		c.muCap.Lock()
		c.capacity = readahead * 3
		c.muCap.Unlock()
	}
	for _, r := range c.readersSnapshot() {
		r.SetReadahead(readahead)
	}
}

func (c *Cache) GetState() *state.CacheState {
	cState := new(state.CacheState)

	piecesState := make(map[int]state.ItemState, 0)
	var fill int64 = 0

	tor := c.getTorrent()
	for _, p := range c.getPieces() {
		size, complete, _ := p.snapshot()
		if size > 0 {
			fill += size
			prio := 0
			if tor != nil {
				prio = int(tor.PieceState(p.Id).Priority)
			}
			piecesState[p.Id] = state.ItemState{
				Id:        p.Id,
				Size:      size,
				Length:    c.pieceLength,
				Completed: complete,
				Priority:  prio,
			}
		}
	}

	readersState := make([]*state.ReaderState, 0)
	now := time.Now().Unix()
	activeReaders := 0

	for _, r := range c.readersSnapshot() {
		rng := r.getPiecesRange()
		pc := r.getReaderPiece()
		inUse := r.isInUse()
		if inUse {
			activeReaders++
		}
		idle := now - r.lastAccessTime()
		if idle < 0 {
			idle = 0
		}
		readersState = append(readersState, &state.ReaderState{
			Start:       rng.Start,
			End:         rng.End,
			Reader:      pc,
			IsUse:       inUse,
			IdleSeconds: idle,
			Readahead:   r.Readahead(),
		})
	}

	c.muAct.Lock()
	lastAct := c.lastActive
	warmPause := c.hadWarmPriority
	c.muAct.Unlock()

	var cacheIdle int64
	if lastAct > 0 && now > lastAct {
		cacheIdle = now - lastAct
	}

	cState.Capacity = c.GetCapacity()
	cState.PiecesLength = c.pieceLength
	cState.PiecesCount = c.pieceCount
	cState.Hash = c.hash.HexString()
	cState.Filled = fill
	cState.ActiveReaders = activeReaders
	cState.IdleSeconds = cacheIdle
	cState.WarmPause = warmPause
	cState.Pieces = piecesState
	cState.Readers = readersState
	return cState
}

func (c *Cache) cleanPieces() {
	if c.isRemove.Load() || c.isClosed.Load() {
		return
	}

	// Protection against concurrent deletion
	if !c.muRemove.TryLock() {
		return // Cleanup is already in progress in another goroutine
	}
	defer c.muRemove.Unlock()

	c.isRemove.Store(true)
	defer func() { c.isRemove.Store(false) }()

	remPieces := c.getRemPieces()
	capLimit := c.GetCapacity()
	if c.filled > capLimit {
		filled := c.filled
		for _, p := range remPieces {
			cost := p.memCost()
			c.removePiece(p)
			filled -= cost
			if filled <= capLimit {
				break
			}
		}
		c.filled = filled
	}
}

func (c *Cache) getRemPieces() []*Piece {
	readers := c.readersSnapshot()
	now := time.Now().Unix()

	hasRecentlyActiveReader := false
	for _, r := range readers {
		if now-r.lastAccessTime() <= 15 {
			hasRecentlyActiveReader = true
			break
		}
	}

	// Collect read ranges from active readers
	ranges := make([]Range, 0, len(readers))
	for _, r := range readers {
		r.checkReader(hasRecentlyActiveReader)
		if r.isInUse() {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	ranges = mergeRange(ranges)

	protectRanges := ranges
	if len(protectRanges) == 0 {
		if lastFile, _, _ := c.activitySnapshot(); lastFile != nil {
			protectRanges = []Range{{File: lastFile}}
		}
	}

	type pieceSortItem struct {
		piece    *Piece
		complete bool
		accessed int64
	}
	candidates := make([]pieceSortItem, 0)
	fill := int64(0)

	// Determine which chunks can be deleted
	for id, p := range c.getPieces() {
		size, complete, accessed := p.snapshot()
		if size > 0 {
			fill += p.memCost()
		}
		if len(ranges) > 0 {
			if !inRanges(ranges, id) {
				if size > 0 && !c.isIdInFileBE(protectRanges, id) {
					candidates = append(candidates, pieceSortItem{p, complete, accessed})
				}
			}
		} else {
			// When preloading or temporarily paused without readers, keep the beginning and end of the active file.
			if size > 0 && !c.isIdInFileBE(protectRanges, id) {
				candidates = append(candidates, pieceSortItem{p, complete, accessed})
			}
		}
	}

	c.filled = fill
	c.reconcilePriorities(ranges, fill)

	// Complete pieces first, oldest first; a piece still downloading goes last. The torrent
	// client never re-requests chunks it has received, so evicting their bytes makes a
	// responsive reader that comes back to them read zeros.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].complete != candidates[j].complete {
			return candidates[i].complete
		}
		return candidates[i].accessed < candidates[j].accessed
	})

	piecesRemove := make([]*Piece, len(candidates))
	for i, item := range candidates {
		piecesRemove[i] = item.piece
	}
	return piecesRemove
}

const (
	prioNone = iota
	prioNormal
	prioHigh
	prioReadahead
	prioNext
	prioNow
)

func setPiecePriorityIfChanged(tp *torrent.Piece, cur torrent.PieceState, want int) {
	switch want {
	case prioNow:
		if cur.Priority != torrent.PiecePriorityNow {
			tp.SetPriority(torrent.PiecePriorityNow)
		}
	case prioNext:
		if cur.Priority != torrent.PiecePriorityNext {
			tp.SetPriority(torrent.PiecePriorityNext)
		}
	case prioReadahead:
		if cur.Priority != torrent.PiecePriorityReadahead {
			tp.SetPriority(torrent.PiecePriorityReadahead)
		}
	case prioHigh:
		if cur.Priority != torrent.PiecePriorityHigh {
			tp.SetPriority(torrent.PiecePriorityHigh)
		}
	case prioNormal:
		if cur.Priority != torrent.PiecePriorityNormal {
			tp.SetPriority(torrent.PiecePriorityNormal)
		}
	default:
		if cur.Priority != torrent.PiecePriorityNone {
			tp.SetPriority(torrent.PiecePriorityNone)
		}
	}
}

func (c *Cache) reconcilePriorities(ranges []Range, fill int64) {
	tor := c.getTorrent()
	if tor == nil || c.preloading.Load() || c.isClosed.Load() {
		return
	}
	pieces := c.getPieces()
	if pieces == nil || c.pieceLength <= 0 {
		return
	}

	readers := c.readersSnapshot()
	activeReaders := make([]*Reader, 0, len(readers))
	for _, r := range readers {
		if r.isInUse() {
			activeReaders = append(activeReaders, r)
		}
	}

	c.muPrio.Lock()
	defer c.muPrio.Unlock()

	capLimit := c.GetCapacity()
	if capLimit <= 0 {
		capLimit = c.pieceLength * 4
	}
	connLimit := 25
	readAheadPct := 95
	if settings.BTsets != nil {
		if settings.BTsets.ConnectionsLimit > 0 {
			connLimit = settings.BTsets.ConnectionsLimit
		}
		if settings.BTsets.ReaderReadAHead > 0 {
			readAheadPct = settings.BTsets.ReaderReadAHead
		}
	}

	desired := make(map[int]int)

	if len(activeReaders) > 0 {
		c.muAct.Lock()
		c.hadWarmPriority = false
		c.muAct.Unlock()

		readerBudget := capLimit / int64(len(activeReaders))
		maxAheadBytes := readerBudget * int64(readAheadPct) / 100
		if maxAheadBytes < c.pieceLength*2 {
			maxAheadBytes = c.pieceLength * 2
		}
		maxPieces := int(maxAheadBytes / c.pieceLength)
		if maxPieces < 2 {
			maxPieces = 2
		}
		count := connLimit / len(activeReaders)
		if count < 2 {
			count = 2
		}
		if count > maxPieces {
			count = maxPieces
		}

		for _, r := range activeReaders {
			readerPos := r.getReaderPiece()
			readerRAHPos := r.getReaderRAHPiece()
			end := r.getPiecesRange().End
			limit := 0
			for i := readerPos; i <= end && i < len(pieces) && limit < count; i++ {
				p := pieces[i]
				if p == nil || p.isComplete() {
					continue
				}
				var prio int
				switch {
				case i == readerPos:
					prio = prioNow
				case i == readerPos+1:
					prio = prioNext
				case i <= readerRAHPos:
					prio = prioReadahead
				case i <= readerRAHPos+5:
					prio = prioHigh
				default:
					prio = prioNormal
				}
				if prio > desired[i] {
					desired[i] = prio
				}
				limit++
			}
		}
	} else {
		// When all HTTP readers temporarily closed (e.g. player paused and closed socket),
		// keep a small warm window ahead of the last playback position if cache has headroom.
		lastFile, lastOff, lastAct := c.activitySnapshot()
		now := time.Now().Unix()
		warmSec := c.pauseWarmWindowSeconds()
		warm := false
		if warmSec > 0 && lastFile != nil && now-lastAct <= warmSec && fill < (capLimit*3)/4 {
			warmBytes := capLimit / 2
			if warmBytes < c.pieceLength*2 {
				warmBytes = c.pieceLength * 2
			}
			if warmBytes > 16<<20 {
				warmBytes = 16 << 20
			}
			startPiece := int((lastOff + lastFile.Offset()) / c.pieceLength)
			endOff := lastOff + warmBytes
			if endOff > lastFile.Length() {
				endOff = lastFile.Length()
			}
			endPiece := int((endOff + lastFile.Offset()) / c.pieceLength)
			warmCount := 0
			for i := startPiece; i <= endPiece && i < len(pieces) && warmCount < 4; i++ {
				p := pieces[i]
				if p == nil || p.isComplete() {
					continue
				}
				prio := prioReadahead
				if i == startPiece {
					prio = prioHigh
				}
				if prio > desired[i] {
					desired[i] = prio
				}
				warmCount++
			}
			warm = warmCount > 0
		}
		c.muAct.Lock()
		c.hadWarmPriority = warm
		c.muAct.Unlock()
	}

	for id := range pieces {
		setPiecePriorityIfChanged(tor.Piece(id), tor.PieceState(id), desired[id])
	}
}

func (c *Cache) isIdInFileBE(ranges []Range, id int) bool {
	if c.pieceLength <= 0 {
		return false
	}
	// keep 8/16 MB
	FileRangeNotDelete := int64(c.pieceLength)
	if FileRangeNotDelete < 8<<20 {
		FileRangeNotDelete = 8 << 20
	}

	for _, rng := range ranges {
		if rng.File == nil {
			continue
		}
		ss := int(rng.File.Offset() / c.pieceLength)
		se := int((rng.File.Offset() + FileRangeNotDelete) / c.pieceLength)

		es := int((rng.File.Offset() + rng.File.Length() - FileRangeNotDelete) / c.pieceLength)
		ee := int((rng.File.Offset() + rng.File.Length()) / c.pieceLength)

		if id >= ss && id < se || id > es && id <= ee {
			return true
		}
	}
	return false
}

//////////////////
// Reader section
////////

func (c *Cache) NewReader(file *torrent.File) *Reader {
	return newReader(file, c)
}

func (c *Cache) GetUseReaders() int {
	if c == nil {
		return 0
	}
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	readers := 0
	for reader := range c.readers {
		if reader.isInUse() {
			readers++
		}
	}
	return readers
}

func (c *Cache) Readers() int {
	if c == nil {
		return 0
	}
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	return len(c.readers)
}

func (c *Cache) CloseReader(r *Reader) {
	r.cache.muReaders.Lock()
	delete(r.cache.readers, r)
	r.cache.muReaders.Unlock()
	// Reader.Close touches anacrolix internals, keep it outside muReaders
	r.Close()
}

func (c *Cache) clearPriority() {
	if c.getTorrent() == nil {
		return
	}
	c.reconcilePriorities(nil, c.FilledBytes())
}

func (c *Cache) GetCapacity() int64 {
	if c == nil {
		return 0
	}
	c.muCap.RLock()
	capLimit := c.capacity
	c.muCap.RUnlock()
	return capLimit
}

func (c *Cache) FilledBytes() int64 {
	if c == nil {
		return 0
	}
	var fill int64
	for _, p := range c.getPieces() {
		fill += p.getSize()
	}
	return fill
}
