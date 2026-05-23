// Package notify implements pluggable notifications for backend status
// transitions. The current implementation supports an SMTP email
// notifier with cooldown / dedupe so a flapping backend doesn't flood
// operators' inboxes.
package notify

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// Event describes a backend status transition that may trigger a
// notification.
type Event struct {
	BackendID string
	BackendName string
	BackendURL  string
	Prev        store.BackendStatus
	Next        store.BackendStatus
	LatencyMS   int64
	Error       string
	Models      []string
	Hostname    string
	At          time.Time
}

// Kind maps the prev/next pair to a notify_on event name.
func (e Event) Kind() string {
	switch e.Next {
	case store.StatusUnhealthy:
		return "backend_unhealthy"
	case store.StatusDegraded:
		return "backend_degraded"
	case store.StatusHealthy:
		if e.Prev != store.StatusHealthy {
			return "backend_recovered"
		}
		return ""
	}
	return ""
}

// Notifier consumes events and dispatches them via the configured
// transport. Dispatch is non-blocking and runs on a background goroutine.
type Notifier struct {
	logger *logging.Logger
	cfg    config.NotificationsConfig
	sender Sender

	mu       sync.Mutex
	lastSent map[string]time.Time // backendID|kind -> when

	ch   chan Event
	stop chan struct{}
	wg   sync.WaitGroup

	// LastResult is exposed so the admin API can show operators whether
	// notifications are working.
	resultMu  sync.Mutex
	lastSendAt time.Time
	lastError  string
}

// Sender is the transport contract; the SMTP sender is the default but
// tests inject a fake.
type Sender interface {
	Send(subject, body string, to []string) error
}

func New(cfg config.NotificationsConfig, logger *logging.Logger) *Notifier {
	n := &Notifier{
		logger:   logger,
		cfg:      cfg,
		lastSent: make(map[string]time.Time),
		ch:       make(chan Event, 64),
		stop:     make(chan struct{}),
	}
	if cfg.Email.Enabled {
		n.sender = newSMTPSender(cfg.Email)
	}
	return n
}

// SetSender overrides the transport (used by tests).
func (n *Notifier) SetSender(s Sender) { n.sender = s }

// Start launches the background consumer.
func (n *Notifier) Start() {
	if !n.cfg.Email.Enabled || n.sender == nil {
		return
	}
	n.wg.Add(1)
	go n.run()
}

func (n *Notifier) Stop() {
	close(n.stop)
	n.wg.Wait()
}

// Notify enqueues an event. Non-blocking: drops the event if the queue
// is full so the caller (e.g. health check loop) is never stalled.
func (n *Notifier) Notify(e Event) {
	if !n.cfg.Email.Enabled || n.sender == nil {
		return
	}
	if !n.matchesNotifyOn(e.Kind()) {
		return
	}
	select {
	case n.ch <- e:
	default:
		n.logger.Warn("notification queue full; dropping event", logging.F(
			"backend_id", e.BackendID, "kind", e.Kind(),
		))
	}
}

func (n *Notifier) matchesNotifyOn(kind string) bool {
	if kind == "" {
		return false
	}
	if len(n.cfg.Email.NotifyOn) == 0 {
		return true
	}
	for _, k := range n.cfg.Email.NotifyOn {
		if k == kind {
			return true
		}
	}
	return false
}

func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case <-n.stop:
			return
		case e := <-n.ch:
			n.dispatch(e)
		}
	}
}

func (n *Notifier) dispatch(e Event) {
	kind := e.Kind()
	dedupeKey := e.BackendID + "|" + kind
	cooldown := time.Duration(n.cfg.Email.CooldownMS) * time.Millisecond
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	// Suppress only when a previous SUCCESSFUL send is still inside the
	// cooldown window. A failed send must not start the cooldown, or the
	// real alert (which is what an operator actually needs) would be
	// silently dropped.
	n.mu.Lock()
	if last, ok := n.lastSent[dedupeKey]; ok && time.Since(last) < cooldown {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	subject := fmt.Sprintf("[llmgateway] %s: %s (%s -> %s)", kind, e.BackendID, e.Prev, e.Next)
	body := buildBody(e, kind)
	if err := n.sender.Send(subject, body, n.cfg.Email.To); err != nil {
		n.logger.Warn("notification send failed", logging.F(
			"backend_id", e.BackendID, "kind", kind, "error", err.Error(),
		))
		n.resultMu.Lock()
		n.lastSendAt = time.Now()
		n.lastError = err.Error()
		n.resultMu.Unlock()
		return
	}
	// Record success — this is the timestamp the cooldown is measured from.
	n.mu.Lock()
	n.lastSent[dedupeKey] = time.Now()
	n.mu.Unlock()
	n.resultMu.Lock()
	n.lastSendAt = time.Now()
	n.lastError = ""
	n.resultMu.Unlock()
}

func buildBody(e Event, kind string) string {
	var sb strings.Builder
	sb.WriteString("Backend status transition\n\n")
	fmt.Fprintf(&sb, "Event:        %s\n", kind)
	fmt.Fprintf(&sb, "Backend ID:   %s\n", e.BackendID)
	fmt.Fprintf(&sb, "Backend name: %s\n", e.BackendName)
	fmt.Fprintf(&sb, "Base URL:     %s\n", e.BackendURL)
	fmt.Fprintf(&sb, "Previous:     %s\n", e.Prev)
	fmt.Fprintf(&sb, "New:          %s\n", e.Next)
	fmt.Fprintf(&sb, "Latency:      %dms\n", e.LatencyMS)
	if e.Error != "" {
		fmt.Fprintf(&sb, "Last error:   %s\n", e.Error)
	}
	if len(e.Models) > 0 {
		fmt.Fprintf(&sb, "Models:       %s\n", strings.Join(e.Models, ", "))
	}
	fmt.Fprintf(&sb, "Gateway host: %s\n", e.Hostname)
	fmt.Fprintf(&sb, "Timestamp:    %s\n", e.At.UTC().Format(time.RFC3339))
	return sb.String()
}

// LastResult returns the most recent send timestamp and error (if any).
// Used by Admin API / Dashboard to show notification health.
func (n *Notifier) LastResult() (time.Time, string) {
	n.resultMu.Lock()
	defer n.resultMu.Unlock()
	return n.lastSendAt, n.lastError
}

// Hostname returns the local hostname for inclusion in notification
// bodies; falls back to "unknown" if os.Hostname fails.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// -- SMTP transport -------------------------------------------------------

type smtpSender struct {
	cfg config.EmailNotifierConfig
}

func newSMTPSender(cfg config.EmailNotifierConfig) *smtpSender { return &smtpSender{cfg: cfg} }

func (s *smtpSender) Send(subject, body string, to []string) error {
	if len(to) == 0 {
		return errors.New("no recipients configured")
	}
	addr := net.JoinHostPort(s.cfg.SMTPHost, fmt.Sprintf("%d", s.cfg.SMTPPort))
	msg := buildMIME(s.cfg.From, to, subject, body)
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.SMTPHost)
	}

	if s.cfg.UseTLS {
		// Implicit TLS (typically port 465). Connection is already
		// encrypted, so tlsAlreadyActive=true.
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.cfg.SMTPHost})
		if err != nil {
			return err
		}
		defer conn.Close()
		return sendOverConn(conn, s.cfg.SMTPHost, auth, s.cfg.From, to, []byte(msg), false, true)
	}
	// Plain TCP, optionally upgrading via STARTTLS.
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	return sendOverConn(conn, s.cfg.SMTPHost, auth, s.cfg.From, to, []byte(msg), s.cfg.StartTLS, false)
}

func sendOverConn(conn net.Conn, host string, auth smtp.Auth, from string, to []string, msg []byte, startTLS bool, tlsAlreadyActive bool) error {
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	tlsActive := tlsAlreadyActive
	if startTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			// Hard fail: operator asked for STARTTLS but the server does
			// not advertise it. Silently downgrading to plaintext would
			// expose SMTP credentials and alert bodies.
			return errors.New("smtp: start_tls=true but server does not advertise STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
		tlsActive = true
	}
	if auth != nil {
		// Refuse to send credentials over an unencrypted channel.
		// Operators who really need plaintext auth must turn off start_tls
		// explicitly (and accept the warning); the default is fail-closed.
		if !tlsActive {
			return errors.New("smtp: refusing to authenticate over plaintext; set use_tls or start_tls")
		}
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err := c.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMIME(from string, to []string, subject, body string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "From: %s\r\n", from)
	fmt.Fprintf(&sb, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&sb, "Subject: %s\r\n", subject)
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return sb.String()
}
