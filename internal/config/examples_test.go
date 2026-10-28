package config

import (
	"path/filepath"
	"testing"
)

// TestExamplesLoad loads the example server.yaml and access.yaml, so a format change cannot leave them behind.
func TestExamplesLoad(t *testing.T) {
	getenv := func(string) string { return "postgres://reader@db.example.internal:5432/sales_eu" }
	cfg, err := Load(filepath.Join("..", "..", "examples", "server.yaml"), getenv)
	if err != nil {
		t.Fatalf("the examples do not load: %v", err)
	}
	if len(cfg.Databases) != 2 || len(cfg.Callers) != 2 {
		t.Errorf("the examples load %d databases and %d callers, want 2 and 2", len(cfg.Databases), len(cfg.Callers))
	}
}
