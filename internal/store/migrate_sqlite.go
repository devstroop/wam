package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// MigrateSQLiteToPostgres copies all app rows from a SQLite file into
// Postgres, tagging them org_default. Idempotent (ON CONFLICT DO NOTHING).
// whatsmeow session tables are NOT copied — re-pair on the new backend.
//
// pgDSN must be an OWNER DSN (runs DDL migrations); the wam_app runtime role
// lacks CREATE privileges. Each table copies inside one transaction in
// ~500-row batches, so a crash rolls back to the last completed table and a
// rerun heals via ON CONFLICT DO NOTHING.
func MigrateSQLiteToPostgres(sqlitePath, pgDSN string) (map[string]int, error) {
	counts := map[string]int{}
	src, err := OpenSQLite(sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("migrate: open sqlite: %w", err)
	}
	defer src.Close()
	dst, err := OpenPostgres(pgDSN)
	if err != nil {
		return nil, fmt.Errorf("migrate: open postgres (owner DSN required): %w", err)
	}
	defer dst.Close()
	if _, err := dst.DefaultOrg(); err != nil {
		return nil, fmt.Errorf("migrate: default org: %w", err)
	}

	const batchSize = 500
	copyTable := func(name, cols, query string, scan func(*sql.Rows) ([]any, error)) error {
		rows, err := src.Query(query)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		defer rows.Close()
		ncols := len(strings.Split(cols, ","))
		n := 0
		flush := func(batch [][]any) error {
			if len(batch) == 0 {
				return nil
			}
			var b strings.Builder
			var args []any
			fmt.Fprintf(&b, `INSERT INTO %s (%s) VALUES `, name, cols)
			for i, row := range batch {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "(")
				for j := range row {
					if j > 0 {
						b.WriteString(",")
					}
					args = append(args, row[j])
					fmt.Fprintf(&b, "$%d", len(args))
				}
				b.WriteString(")")
			}
			b.WriteString(" ON CONFLICT DO NOTHING")
			tx, err := dst.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.Exec(b.String(), args...); err != nil {
				return fmt.Errorf("migrate: write %s: %w", name, err)
			}
			return tx.Commit()
		}
		var batch [][]any
		for rows.Next() {
			args, err := scan(rows)
			if err != nil {
				return fmt.Errorf("migrate: scan %s: %w", name, err)
			}
			if len(args) != ncols {
				return fmt.Errorf("migrate: %s: got %d cols, want %d", name, len(args), ncols)
			}
			batch = append(batch, args)
			n++
			if len(batch) >= batchSize {
				if err := flush(batch); err != nil {
					return err
				}
				batch = nil
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrate: rows %s: %w", name, err)
		}
		if err := flush(batch); err != nil {
			return err
		}
		counts[name] = n
		return nil
	}

	if err := copyTable("contacts", "id, phone, name, wa_jid, wa_ok, created_at, org_id",
		`SELECT id, phone, name, wa_jid, wa_ok, created_at FROM contacts`,
		func(r *sql.Rows) ([]any, error) {
			var id, phone, name, jid, created string
			var ok int
			if err := r.Scan(&id, &phone, &name, &jid, &ok, &created); err != nil {
				return nil, err
			}
			return []any{id, phone, name, jid, ok, created, DefaultOrgID}, nil
		}); err != nil {
		return nil, err
	}
	if err := copyTable("groups", "id, name, color, created_at, org_id",
		`SELECT id, name, color, created_at FROM groups`,
		func(r *sql.Rows) ([]any, error) {
			var id, name, color, created string
			if err := r.Scan(&id, &name, &color, &created); err != nil {
				return nil, err
			}
			return []any{id, name, color, created, DefaultOrgID}, nil
		}); err != nil {
		return nil, err
	}
	if err := copyTable("contact_groups", "contact_id, group_id",
		`SELECT contact_id, group_id FROM contact_groups`,
		func(r *sql.Rows) ([]any, error) {
			var c, g string
			if err := r.Scan(&c, &g); err != nil {
				return nil, err
			}
			return []any{c, g}, nil
		}); err != nil {
		return nil, err
	}
	if err := copyTable("campaigns", "id, name, status, body_template, audience_filter, scheduled_at, created_at, org_id",
		`SELECT id, name, status, body_template, audience_filter, scheduled_at, created_at FROM campaigns`,
		func(r *sql.Rows) ([]any, error) {
			var id, name, status, body, aud, sched, created string
			if err := r.Scan(&id, &name, &status, &body, &aud, &sched, &created); err != nil {
				return nil, err
			}
			return []any{id, name, status, body, aud, sched, created, DefaultOrgID}, nil
		}); err != nil {
		return nil, err
	}
	if err := copyTable("campaign_recipients", "campaign_id, contact_id, status, wa_msg_id, error, sent_at, org_id",
		`SELECT campaign_id, contact_id, status, wa_msg_id, error, sent_at FROM campaign_recipients`,
		func(r *sql.Rows) ([]any, error) {
			var cid, ct, status, msg, errmsg, sent string
			if err := r.Scan(&cid, &ct, &status, &msg, &errmsg, &sent); err != nil {
				return nil, err
			}
			return []any{cid, ct, status, msg, errmsg, sent, DefaultOrgID}, nil
		}); err != nil {
		return nil, err
	}
	if err := copyTable("templates", "id, name, category, language, header, body, footer, created_at, org_id",
		`SELECT id, name, category, language, header, body, footer, created_at FROM templates`,
		func(r *sql.Rows) ([]any, error) {
			var id, name, cat, lang, head, body, foot, created string
			if err := r.Scan(&id, &name, &cat, &lang, &head, &body, &foot, &created); err != nil {
				return nil, err
			}
			return []any{id, name, cat, lang, head, body, foot, created, DefaultOrgID}, nil
		}); err != nil {
		return nil, err
	}
	return counts, nil
}
