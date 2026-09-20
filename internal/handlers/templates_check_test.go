package handlers

import (
	"os"
	"testing"

	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/config"
	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/geoip"
)

// TestTemplatesParse verifies every embedded template parses without error
// (New() calls template.ParseFS at startup) — catches template syntax
// mistakes (e.g. mismatched {{if}}/{{end}}, unknown funcs) at build/test
// time instead of at server startup.
func TestTemplatesParse(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("DATA_DIR", dir)
	os.Setenv("SESSION_SECRET", "x")
	os.Setenv("BASE_URL", "http://localhost:9")
	cfg := config.Load()
	database, err := db.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer database.Close()
	am := auth.NewManager(database, false, false)
	geo := geoip.New()
	if _, err := New(database, cfg, am, geo); err != nil {
		t.Fatalf("template parse error: %v", err)
	}
}
