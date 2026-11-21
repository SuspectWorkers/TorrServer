package api

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"server/log"
	sets "server/settings"
	"server/torr"
	"server/torr/state"
	"server/version"
)

const (
	defaultLogsTailBytes = 2 * 1024 * 1024
	maxLogsTailBytes     = 10 * 1024 * 1024
)

func getLogs(c *gin.Context) {
	tailBytes := defaultLogsTailBytes
	if raw := strings.TrimSpace(c.Query("tail")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			tailBytes = parsed
			if tailBytes > maxLogsTailBytes {
				tailBytes = maxLogsTailBytes
			}
		}
	}

	download := c.Query("download") == "1" || c.Query("download") == "true"
	includeDiag := download || c.Query("diag") == "1" || c.Query("diag") == "true"

	var sb strings.Builder
	if includeDiag {
		writeDiagnosticsHeader(&sb)
		sb.WriteString("\n=== SERVER LOGS ===\n")
	}

	logData := log.ReadLogs(tailBytes)
	if len(logData) > 0 {
		sb.Write(logData)
		if logData[len(logData)-1] != '\n' {
			sb.WriteByte('\n')
		}
	} else {
		sb.WriteString("(no log entries captured yet)\n")
	}

	if download {
		filename := fmt.Sprintf("torrserver-debug-%s.txt", time.Now().UTC().Format("20060102-150405"))
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(sb.String()))
}

func writeDiagnosticsHeader(sb *strings.Builder) {
	sb.WriteString("=== TORRSERVER DIAGNOSTICS ===\n")
	sb.WriteString(fmt.Sprintf("GeneratedUTC: %s\n", time.Now().UTC().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Version: %s (torrent %s)\n", version.Version, version.GetTorrentVersion()))
	sb.WriteString(fmt.Sprintf("Runtime: %s %s/%s CPUs=%d Goroutines=%d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.NumGoroutine()))
	sb.WriteString(fmt.Sprintf("Uptime: %s\n", time.Since(log.StartTime).Round(time.Second)))
	sb.WriteString(fmt.Sprintf("ActiveStreams: %d\n", torr.GetActiveStreams()))

	if sets.BTsets != nil {
		s := sets.BTsets
		sb.WriteString(fmt.Sprintf(
			"Settings: CacheSize=%dMB ReaderReadAHead=%d%% PreloadCache=%d%% UseDisk=%v RemoveCacheOnDrop=%v "+
				"TorrentDisconnectTimeout=%ds PauseTimeout=%ds ConnectionsLimit=%d ResponsiveMode=%v "+
				"DisableTCP=%v DisableUTP=%v DisableDHT=%v DisablePEX=%v DisableUpload=%v EnableIPv6=%v RetrackersMode=%d\n",
			s.CacheSize/(1024*1024), s.ReaderReadAHead, s.PreloadCache, s.UseDisk, s.RemoveCacheOnDrop,
			s.TorrentDisconnectTimeout, s.PauseTimeout, s.ConnectionsLimit, s.ResponsiveMode,
			s.DisableTCP, s.DisableUTP, s.DisableDHT, s.DisablePEX, s.DisableUpload, s.EnableIPv6, s.RetrackersMode,
		))
	}

	torrents := safeListTorrents()
	activeCount := 0
	for _, t := range torrents {
		if t == nil || t.Stat == state.TorrentInDB {
			continue
		}
		activeCount++
	}
	sb.WriteString(fmt.Sprintf("ActiveTorrents: %d (TotalInList: %d)\n", activeCount, len(torrents)))
	for _, t := range torrents {
		if t == nil || t.Stat == state.TorrentInDB {
			continue
		}
		st := t.Status()
		sb.WriteString(fmt.Sprintf(
			"- %s | %q | stat=%s | dl=%.1fKB/s ul=%.1fKB/s | peers=%d/%d (seeds=%d)\n",
			st.Hash, st.Name, st.StatString, st.DownloadSpeed/1024, st.UploadSpeed/1024,
			st.ActivePeers, st.TotalPeers, st.ConnectedSeeders,
		))
		if cs := t.CacheState(); cs != nil {
			sb.WriteString(fmt.Sprintf(
				"  cache: %.2f/%.2fMB pieceLen=%dKB activeReaders=%d idle=%ds warmPause=%v\n",
				float64(cs.Filled)/(1024*1024), float64(cs.Capacity)/(1024*1024), cs.PiecesLength/1024,
				cs.ActiveReaders, cs.IdleSeconds, cs.WarmPause,
			))
			for idx, r := range cs.Readers {
				sb.WriteString(fmt.Sprintf(
					"  reader[%d]: piece=%d range=[%d..%d] inUse=%v idle=%ds readahead=%dKB\n",
					idx, r.Reader, r.Start, r.End, r.IsUse, r.IdleSeconds, r.Readahead/1024,
				))
			}
		}
	}
}

func safeListTorrents() []*torr.Torrent {
	defer func() { _ = recover() }()
	return torr.ListTorrent()
}
