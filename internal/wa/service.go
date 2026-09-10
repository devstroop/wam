// Package wa manages WhatsApp connections for WAM.
//
// Session store: Postgres sqlstore when WAM_DATABASE_URL is set (SaaS path),
// else the legacy SQLite file (separate table prefixes — whatsmeow owns its
// own schema).
//
// Lifecycle: lazy client (no connection at boot) + background auto-connect
// when a stored session exists. Pairing via QR (GetQR) or phone code
// (PairPhone). Sending is rate-limited (30/min token bucket).
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

// Status is the JSON shape for GET /api/v1/connection.
type Status struct {
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"loggedIn"`
	Phone     string `json:"phone,omitempty"`
	PushName  string `json:"pushName,omitempty"`
}

// Service wraps the whatsmeow client.
type Service struct {
	mu          sync.RWMutex
	dbPath      string
	databaseURL string
	log         *slog.Logger
	client      *whatsmeow.Client
	container   *sqlstore.Container
	limiter     *tokenBucket

	// OnReceipt fires for delivery/read receipts (msgIDs, delivered|read).
	// Set by the campaign worker to upgrade recipient states.
	OnReceipt func(msgIDs []string, status string)
}

// JIDForPhone builds a user JID from an E.164 number without a lookup.
func JIDForPhone(phone string) string {
	return NormalizePhone(phone) + "@" + types.DefaultUserServer
}

// New creates the service. It does not connect; call AutoConnect in background.
func New(dbPath string, log *slog.Logger) *Service {
	return NewWithDatabase(dbPath, "", log)
}

// NewWithDatabase creates the service with a Postgres session store when
// databaseURL is set, else the legacy SQLite file. It does not connect;
// call AutoConnect in background.
func NewWithDatabase(dbPath, databaseURL string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{dbPath: dbPath, databaseURL: databaseURL, log: log, limiter: newTokenBucket(30)}
}

// Status snapshots connection state (never blocks).
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{}
	if s.client == nil {
		return st
	}
	st.Connected = s.client.IsConnected()
	st.LoggedIn = s.client.IsLoggedIn()
	if s.client.Store.ID != nil {
		st.Phone = "+" + s.client.Store.ID.User
	}
	if s.client.Store.PushName != "" {
		st.PushName = s.client.Store.PushName
	}
	return st
}

// Connect initialises the client and connects. Idempotent when connected.
func (s *Service) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && s.client.IsConnected() {
		return nil
	}
	if err := s.prepareClientLocked(ctx); err != nil {
		return err
	}
	if err := s.client.Connect(); err != nil {
		return fmt.Errorf("wa: connect: %w", err)
	}
	s.log.Info("whatsapp connected")
	return nil
}

// AutoConnect tries to restore a stored session in the background.
// No stored device → returns nil immediately (pairing UI handles it).
func (s *Service) AutoConnect() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	container, err := s.openContainer(ctx)
	if err != nil {
		s.log.Warn("wa: autoconnect store unavailable", "err", err)
		return
	}
	// Probe-only handle: ownership of the live session passes to s.container
	// inside prepareClientLocked (via Connect below), which opens its own
	// container — so this one must close here, not leak a pool per boot.
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil || device == nil || device.ID == nil {
		s.log.Info("wa: no stored session, waiting for pairing")
		return
	}
	if err := s.Connect(context.Background()); err != nil {
		s.log.Warn("wa: autoconnect failed", "err", err)
		return
	}
}

// EnsureConnected connects if not already connected.
func (s *Service) EnsureConnected(ctx context.Context) error {
	s.mu.RLock()
	ok := s.client != nil && s.client.IsConnected()
	s.mu.RUnlock()
	if ok {
		return nil
	}
	return s.Connect(ctx)
}

// Disconnect closes the connection but keeps the stored session.
func (s *Service) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Disconnect()
		s.client = nil
	}
}

// openContainer opens the sqlstore container: Postgres when databaseURL is
// set (SaaS path, dialect "postgres"), else the legacy SQLite file.
// The DSN must be an OWNER DSN: whatsmeow creates/upgrades its session tables
// at runtime (Upgrade below), which the least-privilege app role cannot do.
// whatsmeow owns its own tables; RLS on app tables does not apply to them.
func (s *Service) openContainer(ctx context.Context) (*sqlstore.Container, error) {
	if strings.TrimSpace(s.databaseURL) != "" {
		db, err := sql.Open("pgx", s.databaseURL)
		if err != nil {
			return nil, fmt.Errorf("wa: open session db: %w", err)
		}
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("wa: ping session db: %w", err)
		}
		c := sqlstore.NewWithDB(db, "postgres", waLog.Noop)
		if err := c.Upgrade(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("wa: upgrade session store: %w", err)
		}
		return c, nil
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", s.dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("wa: open session db: %w", err)
	}
	db.SetMaxOpenConns(1)
	// Dialect "sqlite3": same SQL dialect as modernc's "sqlite" driver.
	c := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := c.Upgrade(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wa: upgrade session store: %w", err)
	}
	return c, nil
}

// prepareClientLocked creates client + store without connecting.
// Caller must hold s.mu. A prior container handle is closed first so
// re-Connect/GetQR cycles don't leak database connections.
func (s *Service) prepareClientLocked(ctx context.Context) error {
	container, err := s.openContainer(ctx)
	if err != nil {
		return err
	}
	if s.container != nil {
		_ = s.container.Close()
	}
	s.container = container
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("wa: get device: %w", err)
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
	client.AddEventHandler(s.handleEvent)
	s.client = client
	return nil
}

// GetQR prepares a fresh client and returns the QR channel.
// Caller takes the first "code" event, renders it, then calls DrainQR.
// Fails fast with "already logged in" when a session exists.
func (s *Service) GetQR(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Disconnect()
		s.client = nil
	}
	if err := s.prepareClientLocked(ctx); err != nil {
		return nil, err
	}
	if s.client.Store.ID != nil {
		return nil, fmt.Errorf("already logged in")
	}
	qrCtx, qrCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ch, err := s.client.GetQRChannel(qrCtx)
	if err != nil {
		qrCancel()
		return nil, fmt.Errorf("wa: qr channel: %w", err)
	}
	if err := s.client.Connect(); err != nil {
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
func (s *Service) QRCodePNG(ctx context.Context) (string, error) {
	ch, err := s.GetQR(ctx)
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

// PairPhone requests a phone-number linking code. It reuses the live client
// when connected-but-not-logged-in, otherwise boots a fresh pairing client.
func (s *Service) PairPhone(ctx context.Context, phone string) (string, error) {
	phone = NormalizePhone(phone)
	if len(phone) < 7 || len(phone) > 15 {
		return "", fmt.Errorf("invalid phone number")
	}
	s.mu.Lock()
	needsBoot := s.client == nil || !s.client.IsConnected()
	if needsBoot {
		if s.client != nil {
			s.client.Disconnect()
			s.client = nil
		}
		if err := s.prepareClientLocked(ctx); err != nil {
			s.mu.Unlock()
			return "", err
		}
		if s.client.Store.ID != nil {
			s.mu.Unlock()
			return "", fmt.Errorf("already logged in")
		}
		if err := s.client.Connect(); err != nil {
			s.mu.Unlock()
			return "", fmt.Errorf("wa: connect: %w", err)
		}
	}
	s.mu.Unlock()

	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return "", fmt.Errorf("wa: client not connected")
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

// Logout tells WhatsApp + wipes the local device, or just wipes local state
// when offline — mirroring notalk's Logout.
func (s *Service) Logout() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		if s.client.IsConnected() {
			err := s.client.Logout(context.Background())
			s.client.Disconnect()
			s.client = nil
			if err != nil {
				return fmt.Errorf("wa: logout: %w", err)
			}
			return nil
		}
		s.client.Disconnect()
		s.client = nil
	}
	ctx := context.Background()
	container, err := s.openContainer(ctx)
	if err != nil {
		return err
	}
	// Cleanup probe only; never stored on s.container.
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
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

// ResolvePhone checks WhatsApp registration and returns the canonical JID.
func (s *Service) ResolvePhone(ctx context.Context, phone string) (string, error) {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil || !c.IsConnected() {
		return "", fmt.Errorf("wa: client not connected")
	}
	norm := NormalizePhone(phone)
	if len(norm) < 7 || len(norm) > 15 {
		return "", fmt.Errorf("invalid phone number %q", phone)
	}
	resp, err := c.IsOnWhatsApp(ctx, []string{"+" + norm})
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

// SendText sends a plain-text message with rate limiting.
func (s *Service) SendText(ctx context.Context, jid, text string) (string, error) {
	if !s.limiter.allow() {
		return "", fmt.Errorf("wa: rate limit exceeded, try again shortly")
	}
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
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

// handleEvent tracks connection lifecycle and forwards delivery/read
// receipts to OnReceipt (campaign funnel upgrades).
func (s *Service) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		s.log.Info("wa: connected event")
	case *events.LoggedOut:
		s.log.Warn("wa: logged out", "reason", v.Reason)
	case *events.PairSuccess:
		s.log.Info("wa: pair success", "id", v.ID.String())
	case *events.Receipt:
		s.log.Debug("wa: receipt", "type", string(v.Type), "count", len(v.MessageIDs))
		var status string
		switch v.Type {
		case types.ReceiptTypeDelivered, types.ReceiptTypeSender:
			status = "delivered"
		case types.ReceiptTypeRead, types.ReceiptTypePlayed:
			status = "read"
		}
		if status != "" && len(v.MessageIDs) > 0 {
			s.mu.RLock()
			hook := s.OnReceipt
			s.mu.RUnlock()
			if hook != nil {
				go hook(v.MessageIDs, status)
			}
		}
	case *events.Message:
		s.log.Debug("wa: message", "chat", v.Info.Chat.String(), "from_me", v.Info.IsFromMe)
	}
}
