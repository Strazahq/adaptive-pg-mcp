package catalog

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// TestTheExampleCatalogLoads loads the catalog that examples/server.yaml names, so a format change cannot leave it behind.
func TestTheExampleCatalogLoads(t *testing.T) {
	getenv := func(string) string { return "postgres://reader@db.example.internal:5432/helpdesk" }
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "server.yaml"), getenv)
	if err != nil {
		t.Fatalf("the example server.yaml does not load: %v", err)
	}
	if cfg.Catalog == "" {
		t.Fatalf("the example server.yaml names no catalog folder")
	}
	entries, reports, err := Load(cfg.Catalog, cfg.Databases, nil)
	if err != nil {
		t.Fatalf("the example catalog does not load: %v", err)
	}
	if len(reports) > 0 {
		t.Errorf("the example catalog has statements that are not tools: %v", reports)
	}
	var names []string
	modes := map[Mode]bool{}
	for _, e := range entries {
		names = append(names, e.Name)
		modes[e.Mode] = true
	}
	if !slices.Equal(names, []string{"close_stale_tickets", "tickets_by_status"}) || !modes[Read] || !modes[Write] {
		t.Errorf("the example catalog holds %v, want close_stale_tickets and tickets_by_status, one write and one read", names)
	}
}
