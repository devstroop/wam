package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Roles (v1: two roles; the roles table seeds these so custom roles slot in
// later without schema changes).
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Permissions (resource:action). Handlers check permissions, never raw
// roles — adding a role later is additive.
const (
	PermOrgManage       = "org:manage"
	PermMembersManage   = "members:manage"
	PermAccountsPair    = "accounts:pair"
	PermAccountsManage  = "accounts:manage"
	PermAccountsView    = "accounts:view"
	PermAccountsUse     = "accounts:use"
	PermContactsManage  = "contacts:manage"
	PermTemplatesManage = "templates:manage"
	PermCampaignsManage = "campaigns:manage"
	PermCampaignsSend   = "campaigns:send"
	PermAnalyticsView   = "analytics:view"
	PermBillingManage   = "billing:manage"
	PermKeysManage      = "keys:manage"
)

// rolePerms mirrors the roles-table seed so per-request checks avoid a join.
// roles table remains the source of truth for future custom roles.
var rolePerms = map[string]map[string]bool{
	RoleAdmin: {"*": true},
	RoleUser: {
		PermContactsManage: true, PermTemplatesManage: true,
		PermCampaignsManage: true, PermCampaignsSend: true,
		PermAnalyticsView: true, PermAccountsView: true, PermAccountsUse: true,
	},
}

// Can reports whether role grants perm. Unknown roles grant nothing.
func Can(role, perm string) bool {
	set, ok := rolePerms[role]
	if !ok {
		return false
	}
	return set["*"] || set[perm]
}

// ValidRole reports whether code is a known v1 role.
func ValidRole(code string) bool { return code == RoleAdmin || code == RoleUser }

// --- Orgs ---

// CreateOrg inserts a tenant (slug derived from name, uniquified). No
// re-read: fail-closed RLS hides the row from unscoped handles (self-serve
// signup has no org context yet), so stamp created_at client-side.
func (db *DB) CreateOrg(name string) (*Org, error) {
	if !db.IsPostgres() {
		return nil, fmt.Errorf("store: orgs require postgres")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("invalid org name")
	}
	slug := slugify(name)
	o := &Org{ID: uuid.NewString(), Name: name, Slug: slug,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(`INSERT INTO orgs (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
			o.ID, o.Name, o.Slug, o.CreatedAt); err != nil {
			if isUniqueErr(err) {
				o.Slug = fmt.Sprintf("%s-%s", slug, uuid.NewString()[:8])
				continue
			}
			return nil, err
		}
		return o, nil
	}
	return nil, fmt.Errorf("could not allocate org slug")
}

// GetOrg fetches one org.
func (db *DB) GetOrg(id string) (*Org, error) {
	o := &Org{}
	if err := db.QueryRow(`SELECT id, name, slug, created_at FROM orgs WHERE id = ?`, id).
		Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return o, nil
}

// OrgIDs lists all tenant ids (owner handle: fail-closed RLS hides orgs from
// unscoped app handles). Used by the sender worker for org iteration.
func (db *DB) OrgIDs() ([]string, error) {
	if !db.IsPostgres() {
		return []string{"local"}, nil
	}
	rows, err := db.Query(`SELECT id FROM orgs ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OrgForMsgID resolves the owning org of a WhatsApp message id (receipt
// routing). Owner handle: bypasses RLS. SQLite has no org_id: ("", true).
func (db *DB) OrgForMsgID(msgID string) (string, bool) {
	if msgID == "" {
		return "", false
	}
	if !db.IsPostgres() {
		return "", true
	}
	var orgID string
	if err := db.QueryRow(`SELECT org_id FROM campaign_recipients WHERE wa_msg_id = ? LIMIT 1`, msgID).Scan(&orgID); err != nil {
		return "", false
	}
	return orgID, true
}

func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "org"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// --- Users ---

// User is an account human. PasswordHash never leaves the store layer.
type User struct {
	ID              string `json:"id"`
	Email           string `json:"email"`
	Name            string `json:"name,omitempty"`
	EmailVerifiedAt string `json:"emailVerifiedAt,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

// normalizeEmail lowercases/trims; "" when malformed.
func normalizeEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 || len(email) < 5 {
		return ""
	}
	return email
}

// CreateUser inserts a user (passwordHash must be a bcrypt hash from
// auth.HashPassword — never plaintext).
func (db *DB) CreateUser(email, name, passwordHash string) (*User, error) {
	if !db.IsPostgres() {
		return nil, fmt.Errorf("store: users require postgres")
	}
	email = normalizeEmail(email)
	if email == "" {
		return nil, fmt.Errorf("invalid email")
	}
	if passwordHash == "" {
		return nil, fmt.Errorf("password required")
	}
	u := &User{ID: uuid.NewString(), Email: email, Name: strings.TrimSpace(name)}
	if _, err := db.Exec(`INSERT INTO users (id, email, name, password_hash) VALUES (?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, passwordHash); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("email already registered")
		}
		return nil, err
	}
	return db.GetUser(u.ID)
}

// GetUser fetches one user (no secrets).
func (db *DB) GetUser(id string) (*User, error) {
	u := &User{}
	if err := db.QueryRow(`SELECT id, email, name, email_verified_at, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return u, nil
}

// GetUserByEmail fetches one user by email (login path, unscoped by design).
func (db *DB) GetUserByEmail(email string) (*User, error) {
	u := &User{}
	if err := db.QueryRow(`SELECT id, email, name, email_verified_at, created_at FROM users WHERE email = ?`, normalizeEmail(email)).
		Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return u, nil
}

// UserPasswordHash loads the bcrypt hash for credential checks (auth only).
func (db *DB) UserPasswordHash(userID string) (string, error) {
	var h string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&h); err != nil {
		return "", err
	}
	return h, nil
}

// SetUserPassword replaces the hash (also revokes sessions — caller).
func (db *DB) SetUserPassword(userID, passwordHash string) error {
	if passwordHash == "" {
		return fmt.Errorf("password required")
	}
	res, err := db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// VerifyUserEmail marks the address verified.
func (db *DB) VerifyUserEmail(userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE users SET email_verified_at = ? WHERE id = ?`, now, userID)
	return err
}

// UserCount counts users (bootstrap seed check).
func (db *DB) UserCount() (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// --- Memberships ---

// Membership binds a user to an org with a role.
type Membership struct {
	UserID    string `json:"userId"`
	OrgID     string `json:"orgId"`
	Role      string `json:"role"`
	UserEmail string `json:"userEmail,omitempty"`
	UserName  string `json:"userName,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// AddMembership inserts a membership (role validated).
func (db *DB) AddMembership(userID, orgID, role string) (*Membership, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	if _, err := db.Exec(`INSERT INTO memberships (user_id, org_id, role) VALUES (?, ?, ?)`, userID, orgID, role); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("already a member")
		}
		return nil, err
	}
	return db.GetMembership(userID, orgID)
}

// GetMembership fetches one membership.
func (db *DB) GetMembership(userID, orgID string) (*Membership, error) {
	m := &Membership{}
	if err := db.QueryRow(`SELECT user_id, org_id, role, created_at FROM memberships WHERE user_id = ? AND org_id = ?`, userID, orgID).
		Scan(&m.UserID, &m.OrgID, &m.Role, &m.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return m, nil
}

// MembershipsByUser lists orgs a user belongs to.
func (db *DB) MembershipsByUser(userID string) ([]Membership, error) {
	rows, err := db.Query(`SELECT user_id, org_id, role, created_at FROM memberships WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.OrgID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MembersByOrg lists an org's members with user details.
func (db *DB) MembersByOrg(orgID string) ([]Membership, error) {
	rows, err := db.Query(`SELECT m.user_id, m.org_id, m.role, u.email, u.name, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? ORDER BY m.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.OrgID, &m.Role, &m.UserEmail, &m.UserName, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// adminCount counts admins in an org.
func (db *DB) adminCount(orgID string) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE org_id = ? AND role = ?`, orgID, RoleAdmin).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// SetMemberRole changes a role. Demoting the last admin is refused.
func (db *DB) SetMemberRole(userID, orgID, role string) (*Membership, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	cur, err := db.GetMembership(userID, orgID)
	if err != nil {
		return nil, err
	}
	if cur.Role == RoleAdmin && role != RoleAdmin {
		n, err := db.adminCount(orgID)
		if err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, fmt.Errorf("cannot demote the last admin")
		}
	}
	if _, err := db.Exec(`UPDATE memberships SET role = ? WHERE user_id = ? AND org_id = ?`, role, userID, orgID); err != nil {
		return nil, err
	}
	return db.GetMembership(userID, orgID)
}

// RemoveMembership removes a member. Removing the last admin is refused.
// Callers revoke the member's org sessions afterwards.
func (db *DB) RemoveMembership(userID, orgID string) error {
	cur, err := db.GetMembership(userID, orgID)
	if err != nil {
		return err
	}
	if cur.Role == RoleAdmin {
		n, err := db.adminCount(orgID)
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("cannot remove the last admin")
		}
	}
	res, err := db.Exec(`DELETE FROM memberships WHERE user_id = ? AND org_id = ?`, userID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Account grants (enforced in feat/multi-account; managed here) ---

// Grant gives a non-admin access to one WA account.
type Grant struct {
	UserID    string `json:"userId"`
	AccountID string `json:"accountId"`
	OrgID     string `json:"orgId"`
	Role      string `json:"role"`
	GrantedBy string `json:"grantedBy,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// SetGrant upserts a grant (role member|viewer).
func (db *DB) SetGrant(userID, accountID, orgID, role, grantedBy string) (*Grant, error) {
	if role != "member" && role != "viewer" {
		return nil, fmt.Errorf("invalid grant role %q", role)
	}
	if db.IsPostgres() {
		_, err := db.Exec(`INSERT INTO wa_account_grants (user_id, account_id, org_id, role, granted_by)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (user_id, account_id)
			DO UPDATE SET org_id = EXCLUDED.org_id, role = EXCLUDED.role, granted_by = EXCLUDED.granted_by`,
			userID, accountID, orgID, role, grantedBy)
		if err != nil {
			return nil, err
		}
	} else {
		if _, err := db.Exec(`INSERT OR REPLACE INTO wa_account_grants (user_id, account_id, org_id, role, granted_by)
			VALUES (?, ?, ?, ?, ?)`, userID, accountID, orgID, role, grantedBy); err != nil {
			return nil, err
		}
	}
	return db.GetGrant(userID, accountID)
}

// GetGrant fetches one grant.
func (db *DB) GetGrant(userID, accountID string) (*Grant, error) {
	g := &Grant{}
	if err := db.QueryRow(`SELECT user_id, account_id, org_id, role, granted_by, created_at
		FROM wa_account_grants WHERE user_id = ? AND account_id = ?`, userID, accountID).
		Scan(&g.UserID, &g.AccountID, &g.OrgID, &g.Role, &g.GrantedBy, &g.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return g, nil
}

// GrantsForUser lists a user's grants in an org (stashed per request).
func (db *DB) GrantsForUser(userID, orgID string) ([]Grant, error) {
	rows, err := db.Query(`SELECT user_id, account_id, org_id, role, granted_by, created_at
		FROM wa_account_grants WHERE user_id = ? AND org_id = ?`, userID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.UserID, &g.AccountID, &g.OrgID, &g.Role, &g.GrantedBy, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RemoveGrant revokes account access.
func (db *DB) RemoveGrant(userID, accountID string) error {
	res, err := db.Exec(`DELETE FROM wa_account_grants WHERE user_id = ? AND account_id = ?`, userID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Sessions ---

// SessionRow is a server-side login session.
type SessionRow struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	OrgID     string `json:"orgId"`
	ExpiresAt string `json:"expiresAt"`
	CreatedAt string `json:"createdAt"`
}

// CreateSession inserts a session row (tokenHash = sha256 of cookie value).
func (db *DB) CreateSession(userID, orgID, tokenHash string, expiresAt time.Time) (*SessionRow, error) {
	s := &SessionRow{ID: uuid.NewString(), UserID: userID, OrgID: orgID,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339)}
	if _, err := db.Exec(`INSERT INTO sessions (id, user_id, org_id, token_hash, expires_at)
		VALUES (?, ?, ?, ?, ?)`, s.ID, s.UserID, s.OrgID, tokenHash, s.ExpiresAt); err != nil {
		return nil, err
	}
	return s, nil
}

// GetSessionByTokenHash loads a live session (revoked/expired → ErrNoRows).
func (db *DB) GetSessionByTokenHash(tokenHash string) (*SessionRow, error) {
	s := &SessionRow{}
	var revoked string
	err := db.QueryRow(`SELECT id, user_id, org_id, expires_at, revoked_at, created_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&s.ID, &s.UserID, &s.OrgID, &s.ExpiresAt, &revoked, &s.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if strings.TrimSpace(revoked) != "" {
		return nil, sql.ErrNoRows
	}
	if t, err := time.Parse(time.RFC3339, s.ExpiresAt); err != nil || time.Now().After(t) {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

// SetSessionOrg moves a session to another org (switcher; membership checked by caller).
func (db *DB) SetSessionOrg(tokenHash, orgID string) error {
	res, err := db.Exec(`UPDATE sessions SET org_id = ? WHERE token_hash = ? AND revoked_at = ''`, orgID, tokenHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RevokeSession logs one device out.
func (db *DB) RevokeSession(tokenHash string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE sessions SET revoked_at = ? WHERE token_hash = ?`, now, tokenHash)
	return err
}

// RevokeUserSessions logs all of a user's devices out (password change).
func (db *DB) RevokeUserSessions(userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at = ''`, now, userID)
	return err
}

// RevokeUserOrgSessions drops a removed member's sessions for that org.
func (db *DB) RevokeUserOrgSessions(userID, orgID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND org_id = ? AND revoked_at = ''`, now, userID, orgID)
	return err
}

// --- Invites ---

// Invite is a pending org invitation. OrgName is denormalized so the public
// accept page can name the workspace without an orgs-table read (fail-closed
// RLS hides orgs without org context; the unguessable token gates access).
type Invite struct {
	ID        string `json:"id"`
	OrgID     string `json:"orgId"`
	OrgName   string `json:"orgName,omitempty"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	InvitedBy string `json:"invitedBy,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// CreateInvite inserts an invitation (tokenHash of the mailed token).
func (db *DB) CreateInvite(orgID, orgName, email, role, tokenHash, invitedBy string, expiresAt time.Time) (*Invite, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	email = normalizeEmail(email)
	if email == "" {
		return nil, fmt.Errorf("invalid email")
	}
	inv := &Invite{ID: uuid.NewString(), OrgID: orgID, OrgName: orgName, Email: email, Role: role,
		InvitedBy: invitedBy, ExpiresAt: expiresAt.UTC().Format(time.RFC3339)}
	if _, err := db.Exec(`INSERT INTO invites (id, org_id, org_name, email, role, token_hash, invited_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, inv.ID, inv.OrgID, inv.OrgName, inv.Email, inv.Role, tokenHash, inv.InvitedBy, inv.ExpiresAt); err != nil {
		return nil, err
	}
	return inv, nil
}

// GetInviteByTokenHash loads a pending, unexpired invite.
func (db *DB) GetInviteByTokenHash(tokenHash string) (*Invite, error) {
	inv := &Invite{}
	var accepted string
	err := db.QueryRow(`SELECT id, org_id, org_name, email, role, invited_by, expires_at, accepted_at, created_at
		FROM invites WHERE token_hash = ?`, tokenHash).
		Scan(&inv.ID, &inv.OrgID, &inv.OrgName, &inv.Email, &inv.Role, &inv.InvitedBy, &inv.ExpiresAt, &accepted, &inv.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if strings.TrimSpace(accepted) != "" {
		return nil, fmt.Errorf("invite already accepted")
	}
	if t, err := time.Parse(time.RFC3339, inv.ExpiresAt); err != nil || time.Now().After(t) {
		return nil, fmt.Errorf("invite expired")
	}
	return inv, nil
}

// AcceptInvite marks consumed (caller adds membership).
func (db *DB) AcceptInvite(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE invites SET accepted_at = ? WHERE id = ?`, now, id)
	return err
}

// InvitesByOrg lists pending invites for an org.
func (db *DB) InvitesByOrg(orgID string) ([]Invite, error) {
	rows, err := db.Query(`SELECT id, org_id, org_name, email, role, invited_by, expires_at, created_at
		FROM invites WHERE org_id = ? AND accepted_at = '' ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.OrgID, &inv.OrgName, &inv.Email, &inv.Role, &inv.InvitedBy, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// --- Password resets ---

// CreatePasswordReset inserts a reset token.
func (db *DB) CreatePasswordReset(userID, tokenHash string, expiresAt time.Time) error {
	_, err := db.Exec(`INSERT INTO password_resets (id, user_id, token_hash, expires_at)
		VALUES (?, ?, ?, ?)`, uuid.NewString(), userID, tokenHash, expiresAt.UTC().Format(time.RFC3339))
	return err
}

// ConsumePasswordReset validates and marks a reset token used, returning userID.
func (db *DB) ConsumePasswordReset(tokenHash string) (string, error) {
	var userID, used, exp string
	err := db.QueryRow(`SELECT user_id, used_at, expires_at FROM password_resets WHERE token_hash = ?`, tokenHash).
		Scan(&userID, &used, &exp)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", sql.ErrNoRows
		}
		return "", err
	}
	if strings.TrimSpace(used) != "" {
		return "", fmt.Errorf("reset already used")
	}
	if t, err := time.Parse(time.RFC3339, exp); err != nil || time.Now().After(t) {
		return "", fmt.Errorf("reset expired")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE password_resets SET used_at = ? WHERE token_hash = ?`, now, tokenHash); err != nil {
		return "", err
	}
	return userID, nil
}

// --- Email verifications ---

// CreateEmailVerification inserts a verify token.
func (db *DB) CreateEmailVerification(userID, tokenHash string, expiresAt time.Time) error {
	_, err := db.Exec(`INSERT INTO email_verifications (id, user_id, token_hash, expires_at)
		VALUES (?, ?, ?, ?)`, uuid.NewString(), userID, tokenHash, expiresAt.UTC().Format(time.RFC3339))
	return err
}

// ConsumeEmailVerification validates a token, marks used+verified.
func (db *DB) ConsumeEmailVerification(tokenHash string) (string, error) {
	var userID, used, exp string
	err := db.QueryRow(`SELECT user_id, used_at, expires_at FROM email_verifications WHERE token_hash = ?`, tokenHash).
		Scan(&userID, &used, &exp)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", sql.ErrNoRows
		}
		return "", err
	}
	if strings.TrimSpace(used) != "" {
		return "", fmt.Errorf("verification already used")
	}
	if t, err := time.Parse(time.RFC3339, exp); err != nil || time.Now().After(t) {
		return "", fmt.Errorf("verification expired")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE email_verifications SET used_at = ? WHERE token_hash = ?`, now, tokenHash); err != nil {
		return "", err
	}
	if err := db.VerifyUserEmail(userID); err != nil {
		return "", err
	}
	return userID, nil
}
