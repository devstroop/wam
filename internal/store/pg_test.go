package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func pgOwnerDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WAM_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAM_DATABASE_URL unset")
	}
	return dsn
}

func pgAppDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WAM_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAM_APP_DATABASE_URL unset")
	}
	return dsn
}

// pgProbeDSN rewrites the owner DSN to a different database name.
func pgProbeDSN(t *testing.T, dbname string) string {
	t.Helper()
	u, err := url.Parse(pgOwnerDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + dbname
	return u.String()
}

// TestPostgresSmoke exercises the SaaS platform path end-to-end (owner DSN).
func TestPostgresSmoke(t *testing.T) {
	dsn := pgOwnerDSN(t)
	db, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	org, err := db.DefaultOrg()
	if err != nil {
		t.Fatalf("default org: %v", err)
	}
	if org.ID != DefaultOrgID {
		t.Fatalf("org id = %q", org.ID)
	}

	c, err := db.CreateContact("+15550137601", "PG Smoke", nil)
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}
	g, err := db.CreateGroup("pg-smoke-group", "red")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := db.UpdateContact(c.ID, "", "", []string{g.ID}, true); err != nil {
		t.Fatalf("membership: %v", err)
	}
	tmpl, err := db.CreateTemplate("pg-smoke-tmpl", "Hi {{name}}", "", "")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	camp, err := db.CreateCampaign("pg-smoke-camp", tmpl.Body, []string{g.ID}, nil, "", "")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if camp.Total != 1 || camp.Queued != 1 {
		t.Fatalf("campaign funnel = %+v", camp)
	}
	if _, _, err := db.ListContacts("", "", 10, ""); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := db.Overview(); err != nil {
		t.Fatalf("overview: %v", err)
	}

	// RLS isolation is covered as the app role in
	// TestPostgresWithOrgIsolation (owner/superuser DSNs bypass RLS, so an
	// isolation assert here would be meaningless).

	if err := db.DeleteCampaign(camp.ID); err != nil {
		t.Fatalf("delete campaign: %v", err)
	}
	if err := db.DeleteContact(c.ID); err != nil {
		t.Fatalf("delete contact: %v", err)
	}
	if err := db.DeleteGroup(g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if err := db.DeleteTemplate(tmpl.ID); err != nil {
		t.Fatalf("delete template: %v", err)
	}
}

// TestPostgresAppRoleCRUD runs the full runtime write path as wam_app inside
// org context, proving the scoped grants cover everything the server does.
func TestPostgresAppRoleCRUD(t *testing.T) {
	pgOwnerDSN(t) // schema must exist; migrations run via owner
	appDB, err := OpenPostgresNoMigrate(pgAppDSN(t))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	defer appDB.Close()

	if err := appDB.WithOrg(context.Background(), DefaultOrgID, func(odb *DB) error {
		if _, err := odb.DefaultOrg(); err != nil {
			return err
		}
		c, err := odb.CreateContact("+15550137602", "App Role", nil)
		if err != nil {
			return err
		}
		g, err := odb.CreateGroup("pg-app-group", "green")
		if err != nil {
			return err
		}
		if _, err := odb.UpdateContact(c.ID, "", "", []string{g.ID}, true); err != nil {
			return err
		}
		tmpl, err := odb.CreateTemplate("pg-app-tmpl", "Hi {{name}}", "", "")
		if err != nil {
			return err
		}
		camp, err := odb.CreateCampaign("pg-app-camp", tmpl.Body, []string{g.ID}, nil, "", "")
		if err != nil {
			return err
		}
		if camp.Total != 1 {
			return errors.New("campaign total != 1")
		}
		if _, err := odb.Overview(); err != nil {
			return err
		}
		for _, cleanup := range []func() error{
			func() error { return odb.DeleteCampaign(camp.ID) },
			func() error { return odb.DeleteContact(c.ID) },
			func() error { return odb.DeleteGroup(g.ID) },
			func() error { return odb.DeleteTemplate(tmpl.ID) },
		} {
			if err := cleanup(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("app role crud: %v", err)
	}
}

// TestPostgresWithOrgIsolation proves RLS bites as wam_app: a foreign org
// sees neither tenant rows nor membership edges (004 policy).
func TestPostgresWithOrgIsolation(t *testing.T) {
	owner, err := OpenPostgres(pgOwnerDSN(t))
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	defer owner.Close()
	appDB, err := OpenPostgresNoMigrate(pgAppDSN(t))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	defer appDB.Close()
	ctx := context.Background()

	if _, err := owner.Exec(`INSERT INTO orgs (id, name, slug) VALUES ('org_other', 'Other', 'other') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed org_other: %v", err)
	}
	c, err := owner.CreateContact("+15550137603", "Isolated", nil)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	g, err := owner.CreateGroup("pg-iso-group", "")
	if err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := owner.UpdateContact(c.ID, "", "", []string{g.ID}, true); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	checkHidden := func(tx *DB) error {
		var dummy string
		if err := tx.QueryRow(`SELECT id FROM contacts WHERE id = ?`, c.ID).Scan(&dummy); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("contacts visible to foreign org: err=%v", err)
		}
		if err := tx.QueryRow(`SELECT contact_id FROM contact_groups WHERE contact_id = ?`, c.ID).Scan(&dummy); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("contact_groups visible to foreign org: err=%v", err)
		}
		rows, err := tx.Query(`SELECT id FROM orgs ORDER BY id`)
		if err != nil {
			t.Errorf("orgs list as foreign org: %v", err)
			return nil
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Errorf("orgs scan: %v", err)
				return nil
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != "org_other" {
			t.Errorf("orgs visible to foreign org = %v, want [org_other]", ids)
		}
		return nil
	}
	if err := appDB.WithOrg(ctx, "org_other", checkHidden); err != nil {
		t.Fatalf("withorg foreign: %v", err)
	}
	if err := appDB.WithOrg(ctx, DefaultOrgID, func(tx *DB) error {
		var dummy string
		if err := tx.QueryRow(`SELECT id FROM contacts WHERE id = ?`, c.ID).Scan(&dummy); err != nil {
			t.Errorf("home org cannot see own row: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("withorg home: %v", err)
	}

	if err := owner.DeleteContact(c.ID); err != nil {
		t.Fatalf("cleanup contact: %v", err)
	}
	if err := owner.DeleteGroup(g.ID); err != nil {
		t.Fatalf("cleanup group: %v", err)
	}
	if _, err := owner.Exec(`DELETE FROM orgs WHERE id = 'org_other'`); err != nil {
		t.Fatalf("cleanup org: %v", err)
	}
}

// TestPostgresAppRoleNoDDL proves the runtime role cannot change schema.
func TestPostgresAppRoleNoDDL(t *testing.T) {
	pgOwnerDSN(t)
	appDB, err := OpenPostgresNoMigrate(pgAppDSN(t))
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	defer appDB.Close()
	if _, err := appDB.Exec(`CREATE TABLE IF NOT EXISTS no_ddl_probe (id TEXT PRIMARY KEY)`); err == nil {
		t.Fatal("wam_app can run DDL, want permission denied")
	}
}

// TestDefaultOrgSQLite proves orgs are Postgres-only.
func TestDefaultOrgSQLite(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "orgs.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.DefaultOrg(); err == nil {
		t.Fatal("DefaultOrg on sqlite succeeded, want error")
	}
}

// TestWithOrgEmpty proves the scoping primitive fails closed: empty orgID
// must error instead of silently matching the RLS permissive-when-unset arm.
func TestWithOrgEmpty(t *testing.T) {
	db, err := OpenPostgres(pgOwnerDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, orgID := range []string{"", "   "} {
		if err := db.WithOrg(context.Background(), orgID, func(*DB) error { return nil }); err == nil {
			t.Fatalf("WithOrg(%q) succeeded, want error", orgID)
		}
	}
}

// TestNoMigrateUnmigrated proves the runtime handle fails fast on a fresh
// database instead of 500ing on first query.
func TestNoMigrateUnmigrated(t *testing.T) {
	owner, err := OpenPostgres(pgOwnerDSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer owner.Close()
	if _, err := owner.Exec(`CREATE DATABASE wam_nomigrate_probe`); err != nil {
		t.Fatalf("create probe db: %v", err)
	}
	defer owner.Exec(`DROP DATABASE IF EXISTS wam_nomigrate_probe`)
	probeDSN := pgProbeDSN(t, "wam_nomigrate_probe")
	if _, err := OpenPostgresNoMigrate(probeDSN); err == nil {
		t.Fatal("NoMigrate on unmigrated DB succeeded, want migrate-schema hint")
	}
}
