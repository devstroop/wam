package store

import "fmt"

// Overview aggregates account-wide marketing stats.
type Overview struct {
	Contacts     int       `json:"contacts"`
	Groups       int       `json:"groups"`
	Campaigns    int       `json:"campaigns"`
	Sent7d       int       `json:"sent7d"`
	DeliveryRate *float64  `json:"deliveryRate"`
	ReadRate     *float64  `json:"readRate"`
	Funnel       Funnel    `json:"funnel"`
	Timeline     []DayStat `json:"timeline"`
}

// DayStat is one day of send outcomes (sent_at date, UTC).
type DayStat struct {
	Day       string `json:"day"`
	Sent      int    `json:"sent"`
	Delivered int    `json:"delivered"`
	Read      int    `json:"read"`
	Failed    int    `json:"failed"`
}

// cutoffExpr returns a SQL expression evaluating to the RFC3339 UTC cutoff
// N days ago, comparable lexicographically against TEXT sent_at.
func (db *DB) cutoffExpr(days int) string {
	if db.IsPostgres() {
		return fmt.Sprintf("to_char(now() AT TIME ZONE 'UTC' - interval '%d days', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')", days)
	}
	return fmt.Sprintf("strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ','now','-%d days')", days)
}

// Overview computes headline stats + 14-day timeline.
func (db *DB) Overview() (Overview, error) {
	var o Overview
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&o.Contacts); err != nil {
		return o, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&o.Groups); err != nil {
		return o, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&o.Campaigns); err != nil {
		return o, err
	}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM campaign_recipients GROUP BY status`)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return o, err
		}
		switch status {
		case RecQueued:
			o.Funnel.Queued = n
		case RecSent:
			o.Funnel.Sent = n
		case RecDelivered:
			o.Funnel.Delivered = n
		case RecRead:
			o.Funnel.Read = n
		case RecReplied:
			o.Funnel.Replied = n
		case RecFailed:
			o.Funnel.Failed = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return o, err
	}

	sent := o.Funnel.Sent + o.Funnel.Delivered + o.Funnel.Read + o.Funnel.Replied
	if sent > 0 {
		dr := float64(o.Funnel.Delivered+o.Funnel.Read+o.Funnel.Replied) / float64(sent)
		o.DeliveryRate = &dr
		if o.Funnel.Delivered+o.Funnel.Read+o.Funnel.Replied > 0 {
			rr := float64(o.Funnel.Read+o.Funnel.Replied) / float64(o.Funnel.Delivered+o.Funnel.Read+o.Funnel.Replied)
			o.ReadRate = &rr
		}
	}
	// sent_at is RFC3339 UTC; lexicographic compare against a matching cutoff works.
	if err := db.QueryRow(`SELECT COUNT(*) FROM campaign_recipients
		WHERE status != 'queued' AND sent_at != '' AND sent_at >= ` + db.cutoffExpr(7)).Scan(&o.Sent7d); err != nil {
		return o, err
	}

	trows, err := db.Query(`SELECT substr(sent_at,1,10) AS day, status, COUNT(*) FROM campaign_recipients
		WHERE sent_at != '' AND sent_at >= ` + db.cutoffExpr(14) + `
		GROUP BY day, status ORDER BY day`)
	if err != nil {
		return o, err
	}
	defer trows.Close()
	byDay := map[string]*DayStat{}
	var order []string
	for trows.Next() {
		var day, status string
		var n int
		if err := trows.Scan(&day, &status, &n); err != nil {
			return o, err
		}
		d, ok := byDay[day]
		if !ok {
			d = &DayStat{Day: day}
			byDay[day] = d
			order = append(order, day)
		}
		switch status {
		case RecSent:
			d.Sent += n
		case RecDelivered:
			d.Delivered += n
		case RecRead, RecReplied:
			d.Read += n
		case RecFailed:
			d.Failed += n
		}
	}
	if err := trows.Err(); err != nil {
		return o, err
	}
	o.Timeline = make([]DayStat, 0, len(order))
	for _, day := range order {
		o.Timeline = append(o.Timeline, *byDay[day])
	}
	return o, nil
}

// ExportRows returns all recipients of a campaign for CSV download.
func (db *DB) ExportRows(campaignID string) ([]Recipient, error) {
	rows, err := db.Query(`SELECT r.contact_id, c.phone, c.name, r.status, r.wa_msg_id, r.error, r.sent_at
		FROM campaign_recipients r JOIN contacts c ON c.id = r.contact_id
		WHERE r.campaign_id = ? ORDER BY c.phone`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var r Recipient
		r.CampaignID = campaignID
		if err := rows.Scan(&r.ContactID, &r.Phone, &r.Name, &r.Status, &r.WAMsgID, &r.Error, &r.SentAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
