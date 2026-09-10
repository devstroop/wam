package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateSQLiteToPostgres verifies the legacy import path.
// Requires WAM_DATABASE_URL.
func TestMigrateSQLiteToPostgres(t *testing.T) {
	dsn := os.Getenv("WAM_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAM_DATABASE_URL unset")
	}
	sqlitePath := filepath.Join(t.TempDir(), "src.db")
	src, err := OpenSQLite(sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	c, err := src.CreateContact("+15550137901", "Migrate Me", nil)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	g, err := src.CreateGroup("migrate-group", "blue")
	if err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := src.UpdateContact(c.ID, "", "", []string{g.ID}, true); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	tmpl, err := src.CreateTemplate("migrate-tmpl", "Hi {{name}}", "", "")
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}
	camp, err := src.CreateCampaign("migrate-camp", tmpl.Body, []string{g.ID}, nil, "", "")
	if err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	_ = src.Close()

	counts, err := MigrateSQLiteToPostgres(sqlitePath, dsn)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if counts["contacts"] != 1 || counts["campaigns"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	// Idempotent rerun.
	if _, err := MigrateSQLiteToPostgres(sqlitePath, dsn); err != nil {
		t.Fatalf("remigrate: %v", err)
	}

	dst, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer dst.Close()
	got, err := dst.GetCampaign(camp.ID)
	if err != nil {
		t.Fatalf("verify campaign: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("campaign total = %d", got.Total)
	}

	if err := dst.DeleteCampaign(camp.ID); err != nil {
		t.Fatalf("cleanup campaign: %v", err)
	}
	if err := dst.DeleteContact(c.ID); err != nil {
		t.Fatalf("cleanup contact: %v", err)
	}
	if err := dst.DeleteGroup(g.ID); err != nil {
		t.Fatalf("cleanup group: %v", err)
	}
	if err := dst.DeleteTemplate(tmpl.ID); err != nil {
		t.Fatalf("cleanup template: %v", err)
	}
}
