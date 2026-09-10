// Package wa manages WhatsApp connections for WAM.
//
// Session store: Postgres sqlstore when databaseURL is set (SaaS path),
// else the legacy SQLite file (separate table prefixes — whatsmeow owns its
// own schema).
//
// Manager holds one whatsmeow client per account. Legacy mode (SQLite) keeps
// the original single-device behavior under the implicit "legacy" account
// (GetFirstDevice). Multi mode (Postgres) maps account rows 1:1 to devices
// via device_jid (GetDevice / NewDevice); unpaired devices don't survive
// restarts by design — re-pair after reboot.
//
// Lifecycle per account: lazy client (no connection at boot) + background
// auto-connect when a stored session exists. Pairing via QR or phone code.
// Sending is rate-limited per account (30/min token bucket).
//
// Callbacks (OnReceipt/OnPaired) fire on whatsmeow event threads under a
// read lock — they must not call back into the Manager (update the store).
package wa

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waCompanionReg "go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func init() {
	// Identity shown in WhatsApp "Linked Devices": phone shows "Chrome (WAM)".
	store.DeviceProps.Os = proto.String("WAM")
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
	store.BaseClientPayload.PushName = proto.String("WAM")
}

// LegacyAccountID is the implicit account in legacy (SQLite) mode.
const LegacyAccountID = "legacy"

// Status is the JSON shape for connection state.
type Status struct {
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"loggedIn"`
	Phone     string `json:"phone,omitempty"`
	PushName  string `json:"pushName,omitempty"`
}

// AccountRef identifies an account for boot-time auto-connect.
type AccountRef struct {
	ID        string
	DeviceJID string
}

type managedClient struct {
	client    *whatsmeow.Client
	limiter   *tokenBucket
	deviceJID string
}

// Manager wraps per-account whatsmeow clients.
type Manager struct {
	mu          sync.RWMutex
	dbPath      string
	databaseURL string
	legacy      bool
	log         *slog.Logger
	container   *sqlstore.Container
	clients     map[string]*managedClient

	// OnReceipt fires for delivery/read receipts (account, msgIDs, delivered|read).
	OnReceipt func(accountID string, msgIDs []string, status string)
	// OnPaired fires after phone/QR pairing with the new device JID so the
	// glue layer can persist it (wa_accounts.device_jid).
	OnPaired func(accountID string, jid string)
}

// JIDForPhone builds a user JID from an E.164 number without a lookup.
func JIDForPhone(phone string) string {
	return NormalizePhone(phone) + "@" + types.DefaultUserServer
}

// NewManager creates the manager. Legacy mode keeps single-device behavior;
// otherwise one client per account. It does not connect; call AutoConnect.
func NewManager(dbPath, databaseURL string, legacy bool, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		dbPath: dbPath, databaseURL: databaseURL, legacy: legacy,
		log: log, clients: map[string]*managedClient{},
	}
}

// normKey maps any account to the legacy key in legacy mode.
func (m *Manager) normKey(accountID string) string {
	if m.legacy {
		return LegacyAccountID
	}
	return accountID
}

// live returns the client if present (never blocks, never builds).
func (m *Manager) live(accountID string) *managedClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[m.normKey(accountID)]
}

// Status snapshots connection state (never blocks).
func (m *Manager) Status(accountID string) Status {
	st := Status{}
	mc := m.live(accountID)
	if mc == nil || mc.client == nil {
		return st
	}
	st.Connected = mc.client.IsConnected()
	st.LoggedIn = mc.client.IsLoggedIn()
	if mc.client.Store.ID != nil {
		st.Phone = "+" + mc.client.Store.ID.User
	}
	if mc.client.Store.PushName != "" {
		st.PushName = mc.client.Store.PushName
	}
	return st
}

// Connect initialises the account client and connects. Idempotent.
func (m *Manager) Connect(ctx context.Context, accountID, deviceJID string) error {
	mc, err := m.clientFor(ctx, accountID, deviceJID)
	if err != nil {
		return err
	}
	if mc.client.IsConnected() {
		return nil
	}
	if err := mc.client.Connect(); err != nil {
		return fmt.Errorf("wa: connect: %w", err)
	}
	m.log.Info("whatsapp connected", "account", m.normKey(accountID))
	return nil
}

// AutoConnect restores stored sessions in the background. Entries without a
// device JID are skipped (pairing UI handles them); legacy mode probes the
// single implicit device and waits when unpaired.
func (m *Manager) AutoConnect(entries []AccountRef) {
	if m.legacy {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		jid, err := m.ProbeFirstDevice(ctx)
		cancel()
		if err != nil {
			m.log.Warn("wa: autoconnect store unavailable", "err", err)
			return
		}
		if jid == "" {
			m.log.Info("wa: no stored session, waiting for pairing")
			return
		}
		entries = []AccountRef{{ID: LegacyAccountID, DeviceJID: jid}}
	}
	for _, e := range entries {
		if e.DeviceJID == "" {
			continue
		}
		if err := m.Connect(context.Background(), e.ID, e.DeviceJID); err != nil {
			m.log.Warn("wa: autoconnect failed", "account", e.ID, "err", err)
		}
	}
}

// EnsureConnected connects if not already connected.
func (m *Manager) EnsureConnected(ctx context.Context, accountID, deviceJID string) error {
	if mc := m.live(accountID); mc != nil && mc.client.IsConnected() {
		return nil
	}
	return m.Connect(ctx, accountID, deviceJID)
}

// Disconnect closes the account connection but keeps the stored session.
func (m *Manager) Disconnect(accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mc, ok := m.clients[m.normKey(accountID)]; ok && mc.client != nil {
		mc.client.Disconnect()
		delete(m.clients, m.normKey(accountID))
	}
}

// ProbeFirstDevice returns the JID of the first stored device, if any.
// Used once at boot to backfill the legacy account row after upgrades.
func (m *Manager) ProbeFirstDevice(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureContainerLocked(ctx); err != nil {
		return "", err
	}
	device, err := m.container.GetFirstDevice(ctx)
	if err != nil || device == nil || device.ID == nil {
		return "", err
	}
	return device.ID.String(), nil
}

// openContainer opens the sqlstore container (own handle).
func (m *Manager) openContainer(ctx context.Context) (*sqlstore.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureContainerLocked(ctx); err != nil {
		return nil, err
	}
	return m.container, nil
}

// ensureContainerLocked opens the shared container once. Caller holds m.mu.
func (m *Manager) ensureContainerLocked(ctx context.Context) error {
	if m.container != nil {
		return nil
	}
	if strings.TrimSpace(m.databaseURL) != "" {
		db, err := sql.Open("pgx", m.databaseURL)
		if err != nil {
			return fmt.Errorf("wa: open session db: %w", err)
		}
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return fmt.Errorf("wa: ping session db: %w", err)
		}
		c := sqlstore.NewWithDB(db, "postgres", waLog.Noop)
		if err := c.Upgrade(ctx); err != nil {
			_ = db.Close()
			return fmt.Errorf("wa: upgrade session store: %w", err)
		}
		m.container = c
		return nil
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", m.dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("wa: open session db: %w", err)
	}
	db.SetMaxOpenConns(1)
	// Dialect "sqlite3": same SQL dialect as modernc's "sqlite" driver.
	c := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := c.Upgrade(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("wa: upgrade session store: %w", err)
	}
	m.container = c
	return nil
}

// clientFor returns the live client, building it on first use. In legacy
// mode the single implicit device is used; otherwise the stored device JID
// selects the device, or a fresh unpaired device is created for pairing.
func (m *Manager) clientFor(ctx context.Context, accountID, deviceJID string) (*managedClient, error) {
	key := m.normKey(accountID)
	if !m.legacy && strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("wa: account required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if mc, ok := m.clients[key]; ok && mc.client != nil {
		return mc, nil
	}
	if err := m.ensureContainerLocked(ctx); err != nil {
		return nil, err
	}
	var device *store.Device
	var err error
	if m.legacy {
		device, err = m.container.GetFirstDevice(ctx)
		if err != nil {
			return nil, fmt.Errorf("wa: get device: %w", err)
		}
	} else if deviceJID != "" {
		jid, perr := types.ParseJID(deviceJID)
		if perr != nil {
			return nil, fmt.Errorf("wa: invalid device jid: %w", perr)
		}
		device, err = m.container.GetDevice(ctx, jid)
		if err != nil {
			return nil, fmt.Errorf("wa: get device: %w", err)
		}
		if device == nil {
			device = m.container.NewDevice()
		}
	} else {
		device = m.container.NewDevice()
	}
	if device.PushName == "" {
		device.PushName = "WAM"
	}
	client := whatsmeow.NewClient(device, waLog.Noop)
	// Force IPv4 transport (same workaround as notalk for flaky IPv6).
	dialer := &net.Dialer{}
	client.SetWebsocketHTTPClient(&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2: true,
	}})
	client.AddEventHandler(m.handlerFor(key))
	mc := &managedClient{client: client, limiter: newTokenBucket(30), deviceJID: deviceJID}
	m.clients[key] = mc
	return mc, nil
}

// GetQR prepares the account client and returns the QR channel.
// Fails fast with "already logged in" when a session exists.
func (m *Manager) GetQR(ctx context.Context, accountID, deviceJID string) (<-chan whatsmeow.QRChannelItem, error) {
	m.Disconnect(accountID)
	mc, err := m.clientFor(ctx, accountID, deviceJID)
	if err != nil {
		return nil, err
	}
	if mc.client.Store.ID != nil {
		return nil, fmt.Errorf("already logged in")
	}
	qrCtx, qrCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ch, err := mc.client.GetQRChannel(qrCtx)
	if err != nil {
		qrCancel()
		return nil, fmt.Errorf("wa: qr channel: %w", err)
	}
	if err := mc.client.Connect(); err != nil {
		qrCancel()
		return nil, fmt.Errorf("wa: connect: %w", err)
	}
	wrapped := make(chan whatsmeow.QRChannelItem, 1)
	go func() {
		defer qrCancel()
		for item := range ch {
			wrapped <- item
		}
		close(wrapped)
	}()
	return wrapped, nil
}

// DrainQR consumes leftover QR events so the client never stalls.
func DrainQR(ch <-chan whatsmeow.QRChannelItem) {
	go func() {
		for range ch {
		}
	}()
}

// QRCodePNG blocks until the next QR "code" event (or ctx timeout) and
// returns it as a PNG data URL. Remaining events are drained.
func (m *Manager) QRCodePNG(ctx context.Context, accountID, deviceJID string) (string, error) {
	ch, err := m.GetQR(ctx, accountID, deviceJID)
	if err != nil {
		return "", err
	}
	defer DrainQR(ch)
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wa: qr timeout: %w", ctx.Err())
		case item, ok := <-ch:
			if !ok {
				return "", fmt.Errorf("wa: qr channel closed")
			}
			if item.Event == "code" && item.Code != "" {
				png, err := qrcode.Encode(item.Code, qrcode.Medium, 256)
				if err != nil {
					return "", fmt.Errorf("wa: qr encode: %w", err)
				}
				return "data:image/png;base64," + b64(png), nil
			}
			if item.Event == "success" {
				return "", fmt.Errorf("already logged in")
			}
		}
	}
}

// PairPhone requests a phone-number linking code for the account.
func (m *Manager) PairPhone(ctx context.Context, accountID, deviceJID, phone string) (string, error) {
	phone = NormalizePhone(phone)
	if len(phone) < 7 || len(phone) > 15 {
		return "", fmt.Errorf("invalid phone number")
	}
	mc, err := m.clientFor(ctx, accountID, deviceJID)
	if err != nil {
		return "", err
	}
	// Lock only for the connect check; PairPhone network call runs unlocked.
	m.mu.RLock()
	c := mc.client
	connected := c.IsConnected()
	m.mu.RUnlock()
	if !connected {
		if err := m.Connect(ctx, accountID, deviceJID); err != nil {
			return "", err
		}
		m.mu.RLock()
		c = mc.client
		m.mu.RUnlock()
	}
	if c.Store.ID != nil {
		return "", fmt.Errorf("already logged in")
	}
	code, err := c.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", fmt.Errorf("wa: pair phone: %w", err)
	}
	return code, nil
}

// RemoveDevice logs out and wipes the account device, mirroring the old
// single-device Logout (online Logout, else local device delete).
func (m *Manager) RemoveDevice(accountID, deviceJID string) error {
	key := m.normKey(accountID)
	m.mu.Lock()
	mc, ok := m.clients[key]
	if ok && mc.client != nil && mc.client.IsConnected() {
		c := mc.client
		m.mu.Unlock()
		err := c.Logout(context.Background())
		c.Disconnect()
		m.mu.Lock()
		delete(m.clients, key)
		m.mu.Unlock()
		if err != nil {
			return fmt.Errorf("wa: logout: %w", err)
		}
		return nil
	}
	if ok {
		delete(m.clients, key)
	}
	m.mu.Unlock()

	ctx := context.Background()
	m.mu.Lock()
	if err := m.ensureContainerLocked(ctx); err != nil {
		m.mu.Unlock()
		return err
	}
	container := m.container
	m.mu.Unlock()
	jidStr := deviceJID
	if jidStr == "" && m.legacy {
		device, err := container.GetFirstDevice(ctx)
		if err != nil {
			return fmt.Errorf("wa: get device for cleanup: %w", err)
		}
		if device == nil || device.ID == nil {
			return nil
		}
		if err := device.Delete(ctx); err != nil {
			return fmt.Errorf("wa: delete stored device: %w", err)
		}
		return nil
	}
	if jidStr == "" {
		return nil
	}
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return fmt.Errorf("wa: invalid device jid: %w", err)
	}
	device, err := container.GetDevice(ctx, jid)
	if err != nil {
		return fmt.Errorf("wa: get device for cleanup: %w", err)
	}
	if device != nil && device.ID != nil {
		if err := device.Delete(ctx); err != nil {
			return fmt.Errorf("wa: delete stored device: %w", err)
		}
	}
	return nil
}

// ResolvePhone checks WhatsApp registration via the account's client.
func (m *Manager) ResolvePhone(ctx context.Context, accountID, phone string) (string, error) {
	mc := m.live(accountID)
	if mc == nil || !mc.client.IsConnected() {
		return "", fmt.Errorf("wa: client not connected")
	}
	norm := NormalizePhone(phone)
	if len(norm) < 7 || len(norm) > 15 {
		return "", fmt.Errorf("invalid phone number %q", phone)
	}
	resp, err := mc.client.IsOnWhatsApp(ctx, []string{"+" + norm})
	if err != nil {
		return "", fmt.Errorf("wa: phone lookup: %w", err)
	}
	for _, r := range resp {
		if r.IsIn {
			return r.JID.String(), nil
		}
	}
	return "", fmt.Errorf("phone %s is not on WhatsApp", norm)
}

// SendText sends a plain-text message with per-account rate limiting.
func (m *Manager) SendText(ctx context.Context, accountID, jid, text string) (string, error) {
	mc := m.live(accountID)
	if mc == nil {
		return "", fmt.Errorf("wa: client not connected")
	}
	if !mc.limiter.allow() {
		return "", fmt.Errorf("wa: rate limit exceeded, try again shortly")
	}
	c := mc.client
	if c == nil || !c.IsConnected() {
		return "", fmt.Errorf("wa: client not connected")
	}
	target, err := types.ParseJID(jid)
	if err != nil {
		return "", fmt.Errorf("wa: invalid jid %q: %w", jid, err)
	}
	resp, err := c.SendMessage(ctx, target, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return "", fmt.Errorf("wa: send: %w", err)
	}
	return resp.ID, nil
}

// handlerFor binds account context into the event handler.
func (m *Manager) handlerFor(accountID string) func(interface{}) {
	return func(evt interface{}) {
		m.handleEvent(accountID, evt)
	}
}

// handleEvent tracks connection lifecycle and forwards delivery/read
// receipts to OnReceipt; pairing success fires OnPaired for persistence.
func (m *Manager) handleEvent(accountID string, evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		m.log.Info("wa: connected event", "account", accountID)
	case *events.LoggedOut:
		m.log.Warn("wa: logged out", "account", accountID, "reason", v.Reason)
	case *events.PairSuccess:
		jid := v.ID.String()
		m.log.Info("wa: pair success", "account", accountID, "id", jid)
		m.mu.Lock()
		if mc, ok := m.clients[accountID]; ok {
			mc.deviceJID = jid
		}
		hook := m.OnPaired
		m.mu.Unlock()
		if hook != nil {
			go hook(accountID, jid)
		}
	case *events.Receipt:
		m.log.Debug("wa: receipt", "account", accountID, "type", string(v.Type), "count", len(v.MessageIDs))
		var status string
		switch v.Type {
		case types.ReceiptTypeDelivered, types.ReceiptTypeSender:
			status = "delivered"
		case types.ReceiptTypeRead, types.ReceiptTypePlayed:
			status = "read"
		}
		if status != "" && len(v.MessageIDs) > 0 {
			m.mu.RLock()
			hook := m.OnReceipt
			m.mu.RUnlock()
			if hook != nil {
				go hook(accountID, v.MessageIDs, status)
			}
		}
	case *events.Message:
		m.log.Debug("wa: message", "account", accountID, "chat", v.Info.Chat.String(), "from_me", v.Info.IsFromMe)
	}
}
