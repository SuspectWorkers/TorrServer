package torrstor

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

func TestMemPieceBufferPoolClearsAndReuses(t *testing.T) {
	c := NewCache(4<<10, nil)
	c.pieceLength = 1 << 10
	c.bufPool.Store(&sync.Pool{
		New: func() any {
			return make([]byte, c.pieceLength)
		},
	})

	p0 := &Piece{Id: 0, cache: c}
	p0.mPiece = NewMemPiece(p0)
	p1 := &Piece{Id: 1, cache: c}
	p1.mPiece = NewMemPiece(p1)

	dirty := bytes.Repeat([]byte{0xab}, 512)
	if _, err := p0.WriteAt(dirty, 512); err != nil {
		t.Fatal(err)
	}
	p0.mPiece.Release()

	// Allocate next piece; even if the pool returns p0's buffer, bytes outside the
	// newly written region must be zeroed so stale data cannot leak.
	if _, err := p1.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, 512)
	if _, err := p1.ReadAt(tail, 512); err != nil {
		t.Fatal(err)
	}
	for i, b := range tail {
		if b != 0 {
			t.Fatalf("stale byte at offset %d: got 0x%x, want 0", 512+i, b)
		}
	}
	p1.mPiece.Release()
}

func TestCheckReaderPauseGraceVsSecondaryIdleParking(t *testing.T) {
	c := NewCache(4<<20, nil)
	c.pieceLength = 1 << 10

	now := time.Now().Unix()
	sole := &Reader{
		Reader:     &fakeTorrentReader{pos: 128 << 10},
		offset:     128 << 10,
		cache:      c,
		isUse:      true,
		lastAccess: now - 120, // paused for 2 minutes
	}
	c.readers[sole] = struct{}{}

	sole.checkReader(false)
	if !sole.isInUse() {
		t.Fatal("sole reader paused for 2 minutes was parked unexpectedly")
	}

	// Add an actively reading second reader: the idle reader (>90s) should now yield cache.
	active := &Reader{
		Reader:     &fakeTorrentReader{pos: 256 << 10},
		offset:     256 << 10,
		cache:      c,
		isUse:      true,
		lastAccess: now - 2,
	}
	c.readers[active] = struct{}{}

	sole.checkReader(true)
	if sole.isInUse() {
		t.Fatal("idle secondary reader was not parked while another reader is active")
	}
	active.checkReader(true)
	if !active.isInUse() {
		t.Fatal("active reader was parked")
	}

	// Remove active reader and age sole past 5 minutes -> must park.
	delete(c.readers, active)
	sole.readerOn()
	sole.mu.Lock()
	sole.lastAccess = now - 305
	sole.mu.Unlock()
	sole.checkReader(false)
	if sole.isInUse() {
		t.Fatal("abandoned reader idle >300s was not parked")
	}
}

func openTestTorrentForCache(t *testing.T, cacheCap int64, pieceLen int64, totalLen int64) (*torrent.Client, *torrent.Torrent, *Cache) {
	t.Helper()
	stor := NewStorage(cacheCap)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = stor
	cfg.DataDir = t.TempDir()
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.NoDefaultPortForwarding = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	numPieces := int((totalLen + pieceLen - 1) / pieceLen)
	info := metainfo.Info{
		Name:        "movie.mkv",
		PieceLength: pieceLen,
		Length:      totalLen,
		Pieces:      make([]byte, 20*numPieces),
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		cl.Close()
		t.Fatal(err)
	}
	tor, err := cl.AddTorrent(&metainfo.MetaInfo{InfoBytes: infoBytes})
	if err != nil {
		cl.Close()
		t.Fatal(err)
	}
	<-tor.GotInfo()
	c := stor.GetCache(tor.InfoHash())
	if c == nil {
		cl.Close()
		t.Fatal("missing cache")
	}
	c.SetTorrent(tor)
	t.Cleanup(func() {
		cl.Close()
		if c.loopDone != nil {
			<-c.loopDone
		}
	})
	return cl, tor, c
}

func syncClean(c *Cache) {
	c.muRemove.Lock()
	defer c.muRemove.Unlock()
	c.getRemPieces()
}

func TestReconcilePrioritiesStartOfFileAndBoundedByCacheBudget(t *testing.T) {
	initTestSettings(t)
	prevConn := settings.BTsets.ConnectionsLimit
	prevRAH := settings.BTsets.ReaderReadAHead
	settings.BTsets.ConnectionsLimit = 25
	settings.BTsets.ReaderReadAHead = 95
	t.Cleanup(func() {
		settings.BTsets.ConnectionsLimit = prevConn
		settings.BTsets.ReaderReadAHead = prevRAH
	})

	// Cache capacity = 64 KiB, pieceLen = 16 KiB, 20 pieces (320 KiB total).
	// Budget allows ~3 pieces ahead, NOT 25 pieces.
	_, tor, c := openTestTorrentForCache(t, 64<<10, 16<<10, 320<<10)
	r := c.NewReader(tor.Files()[0])
	defer c.CloseReader(r)
	r.SetReadahead(32 << 10)

	syncClean(c)

	if got := tor.PieceState(0).Priority; got != torrent.PiecePriorityNow {
		t.Fatalf("piece 0 priority = %v, want PiecePriorityNow", got)
	}
	if got := tor.PieceState(1).Priority; got != torrent.PiecePriorityNext {
		t.Fatalf("piece 1 priority = %v, want PiecePriorityNext", got)
	}

	prioCount := 0
	for i := 0; i < c.pieceCount; i++ {
		if tor.PieceState(i).Priority != torrent.PiecePriorityNone {
			prioCount++
		}
	}
	if prioCount > 4 {
		t.Fatalf("prioritized %d pieces, expected <= 4 for 64KiB cache budget", prioCount)
	}
}

func TestReconcilePrioritiesPauseWarmWindowAndExpiry(t *testing.T) {
	initTestSettings(t)
	prevConn := settings.BTsets.ConnectionsLimit
	prevRAH := settings.BTsets.ReaderReadAHead
	settings.BTsets.ConnectionsLimit = 25
	settings.BTsets.ReaderReadAHead = 95
	t.Cleanup(func() {
		settings.BTsets.ConnectionsLimit = prevConn
		settings.BTsets.ReaderReadAHead = prevRAH
	})

	_, tor, c := openTestTorrentForCache(t, 128<<10, 16<<10, 512<<10)
	r := c.NewReader(tor.Files()[0])
	// Simulate playback at 64 KiB (piece 4), then player closes socket on pause.
	c.touchActivity(tor.Files()[0], 64<<10, time.Now().Unix())
	c.CloseReader(r)

	syncClean(c)

	warmSet := 0
	for i := 4; i < 10; i++ {
		if tor.PieceState(i).Priority != torrent.PiecePriorityNone {
			warmSet++
		}
	}
	if warmSet == 0 {
		t.Fatal("expected warm piece priorities to remain ahead of paused offset")
	}

	// Expire activity past defaultPauseWarmSeconds -> priorities must clear cleanly.
	c.touchActivity(tor.Files()[0], 64<<10, time.Now().Unix()-defaultPauseWarmSeconds-10)
	syncClean(c)

	for i := 0; i < c.pieceCount; i++ {
		if p := tor.PieceState(i).Priority; p != torrent.PiecePriorityNone {
			t.Fatalf("piece %d still has priority %v after warm window expired", i, p)
		}
	}
}

func TestDiskPieceReusesFileHandleAndBoundsOpenFiles(t *testing.T) {
	initTestSettings(t)
	prevDisk := settings.BTsets.UseDisk
	prevPath := settings.BTsets.TorrentsSavePath
	prevDrop := settings.BTsets.RemoveCacheOnDrop
	tmp := t.TempDir()
	settings.BTsets.UseDisk = true
	settings.BTsets.TorrentsSavePath = tmp
	settings.BTsets.RemoveCacheOnDrop = true
	t.Cleanup(func() {
		settings.BTsets.UseDisk = prevDisk
		settings.BTsets.TorrentsSavePath = prevPath
		settings.BTsets.RemoveCacheOnDrop = prevDrop
	})

	c := NewCache(64<<20, nil)
	c.pieceLength = 64 << 10
	c.pieceCount = 50
	c.hash = metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	if err := os.MkdirAll(filepath.Join(tmp, c.hash.HexString()), 0o777); err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte{0x5c}, 16<<10)
	for i := 0; i < c.pieceCount; i++ {
		p := NewPiece(i, c)
		c.pieces[i] = p
		for off := int64(0); off < 64<<10; off += 16 << 10 {
			if _, err := p.WriteAt(payload, off); err != nil {
				t.Fatalf("piece %d WriteAt(%d): %v", i, off, err)
			}
		}
		if open := c.openDiskFilesCount(); open > maxOpenDiskFiles {
			t.Fatalf("open disk files = %d, exceeds maxOpenDiskFiles=%d", open, maxOpenDiskFiles)
		}
	}

	// Read back from an earlier evicted piece (must reopen seamlessly) and a hot piece.
	got := make([]byte, 16<<10)
	for _, idx := range []int{0, 49} {
		if _, err := c.pieces[idx].ReadAt(got, 0); err != nil {
			t.Fatalf("piece %d ReadAt: %v", idx, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("piece %d data mismatch", idx)
		}
	}

	// Close must close all open file handles before removing piece files (critical on Windows).
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if open := c.openDiskFilesCount(); open != 0 {
		t.Fatalf("open disk files after Close = %d, want 0", open)
	}
	if _, err := os.Stat(filepath.Join(tmp, c.hash.HexString(), "0")); !os.IsNotExist(err) {
		t.Fatalf("expected piece file 0 to be removed on Close with RemoveCacheOnDrop=true, err=%v", err)
	}
}

func TestCacheGetStateDiagnostics(t *testing.T) {
	initTestSettings(t)
	prevConn := settings.BTsets.ConnectionsLimit
	prevRAH := settings.BTsets.ReaderReadAHead
	prevPause := settings.BTsets.PauseTimeout
	settings.BTsets.ConnectionsLimit = 25
	settings.BTsets.ReaderReadAHead = 95
	settings.BTsets.PauseTimeout = 240
	t.Cleanup(func() {
		settings.BTsets.ConnectionsLimit = prevConn
		settings.BTsets.ReaderReadAHead = prevRAH
		settings.BTsets.PauseTimeout = prevPause
	})

	_, tor, c := openTestTorrentForCache(t, 128<<10, 16<<10, 512<<10)
	r := c.NewReader(tor.Files()[0])
	r.SetReadahead(32 << 10)
	st := c.GetState()
	if st.ActiveReaders != 1 || len(st.Readers) != 1 || !st.Readers[0].IsUse {
		t.Fatalf("unexpected active reader state: %+v", st)
	}
	if st.Readers[0].Readahead != 32<<10 {
		t.Fatalf("reader readahead = %d, want %d", st.Readers[0].Readahead, 32<<10)
	}

	c.touchActivity(tor.Files()[0], 16<<10, time.Now().Unix())
	c.CloseReader(r)
	syncClean(c)
	st = c.GetState()
	if st.ActiveReaders != 0 || !st.WarmPause {
		t.Fatalf("expected ActiveReaders=0 and WarmPause=true during pause grace, got ActiveReaders=%d WarmPause=%v", st.ActiveReaders, st.WarmPause)
	}
}
