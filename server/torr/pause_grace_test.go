package torr

import (
	"testing"
	"time"

	"server/settings"
)

func TestDisconnectTimeoutRespectsPauseGraceAndLargeConfig(t *testing.T) {
	initTestGlobals(t)
	prev := settings.BTsets.TorrentDisconnectTimeout
	prevPause := settings.BTsets.PauseTimeout
	t.Cleanup(func() {
		settings.BTsets.TorrentDisconnectTimeout = prev
		settings.BTsets.PauseTimeout = prevPause
	})

	tor := &Torrent{}

	settings.BTsets.TorrentDisconnectTimeout = 30
	settings.BTsets.PauseTimeout = 180
	if got := tor.disconnectTimeout(); got != 30*time.Second {
		t.Fatalf("pre-stream disconnectTimeout = %v, want 30s", got)
	}

	// Once streamed, default 30s gets configured PauseTimeout (180s).
	tor.hasStreamed.Store(true)
	if got := tor.disconnectTimeout(); got != 180*time.Second {
		t.Fatalf("streamed disconnectTimeout = %v, want 180s", got)
	}

	// Custom PauseTimeout = 420s extends grace period.
	settings.BTsets.PauseTimeout = 420
	if got := tor.disconnectTimeout(); got != 420*time.Second {
		t.Fatalf("custom 420s PauseTimeout disconnectTimeout = %v, want 420s", got)
	}

	// PauseTimeout = -1 disables extra grace period.
	settings.BTsets.PauseTimeout = -1
	if got := tor.disconnectTimeout(); got != 30*time.Second {
		t.Fatalf("disabled PauseTimeout disconnectTimeout = %v, want 30s", got)
	}

	// Larger TorrentDisconnectTimeout must not be clamped to 1 minute.
	settings.BTsets.TorrentDisconnectTimeout = 300
	if got := tor.disconnectTimeout(); got != 300*time.Second {
		t.Fatalf("configured 300s disconnectTimeout = %v, want 300s", got)
	}
}

func TestBTServerConfigureEnablesSeedWhenUploadAllowed(t *testing.T) {
	initTestGlobals(t)
	prevDisableUpload := settings.BTsets.DisableUpload
	prevUploadLimit := settings.BTsets.UploadRateLimit
	t.Cleanup(func() {
		settings.BTsets.DisableUpload = prevDisableUpload
		settings.BTsets.UploadRateLimit = prevUploadLimit
	})

	settings.BTsets.DisableUpload = false
	settings.BTsets.UploadRateLimit = 0

	bt := NewBTS()
	bt.configure()
	if !bt.config.Seed {
		t.Fatal("expected Seed=true when DisableUpload=false so peers stay connected during pauses")
	}

	settings.BTsets.DisableUpload = true
	bt.configure()
	if bt.config.Seed || !bt.config.NoUpload {
		t.Fatalf("expected Seed=false and NoUpload=true when DisableUpload=true, got Seed=%v NoUpload=%v", bt.config.Seed, bt.config.NoUpload)
	}
}
