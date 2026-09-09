package dolt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
)

// TestApplyResolvedConfigReadsPoolKeysWithoutInitialize pins the config.yaml
// rung of the pool ladder for library consumers. The root package (what an
// orchestrator links) reaches applyResolvedConfig without ever calling
// config.Initialize, so the global config getter is empty and the .beads
// directory's own config.yaml is the only place a knob can come from
// (gastownhall/beads#6443). The deadline keys gained that fallback with the
// ladder fix for #6144; dolt.max-conns is read in the same function and has
// to take the same route.
func TestApplyResolvedConfigReadsPoolKeysWithoutInitialize(t *testing.T) {
	t.Setenv("BEADS_DOLT_POOL_READ_TIMEOUT", "")
	t.Setenv("BEADS_DOLT_POOL_WRITE_TIMEOUT", "")
	t.Setenv("BEADS_DOLT_MAX_CONNS", "")
	config.ResetForTesting()
	t.Cleanup(config.ResetForTesting)

	beadsDir := t.TempDir()
	yaml := "dolt:\n  pool-read-timeout: 300s\n  max-conns: 25\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	if err := applyResolvedConfig(context.Background(), beadsDir, &configfile.Config{}, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	if cfg.PoolReadTimeout != 300*time.Second {
		t.Fatalf("PoolReadTimeout = %v, want 300s from config.yaml without config.Initialize", cfg.PoolReadTimeout)
	}
	if cfg.MaxOpenConns != 25 {
		t.Fatalf("MaxOpenConns = %d, want 25 from config.yaml without config.Initialize", cfg.MaxOpenConns)
	}
}
