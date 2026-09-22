// Package smtp implements a Notifier that mails run events through an SMTP
// server as plain-text messages.
//
// The transport is the standard library's net/smtp: no new dependency, same as
// telegram and webhook riding on bare net/http. That choice has one visible
// edge — net/smtp speaks AUTH PLAIN and not AUTH LOGIN, which some older
// corporate servers still require. Such a server is rejected with the list of
// mechanisms it advertised rather than silently failing to authenticate.
package smtp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/mail"
	netsmtp "net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Notifiers.Register("smtp", New)
}

// TLS modes. The mode is explicit rather than inferred from the port: guessing
// wrong here means either a failed send or a password on the wire.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "implicit"
	TLSNone     = "none"
)

// sendTimeout caps a whole SMTP conversation — dial, handshake, auth and data.
// Delivery runs on a detached context, so without a deadline of its own a hung
// server would pin a worker indefinitely.
const sendTimeout = 10 * time.Second

// subjectLimit bounds the subject before RFC 2047 encoding. A dump failure can
// carry a kilobyte of stderr, and that has no business being a header.
const subjectLimit = 120

// Config is the smtp notifier config. password_ref is resolved upstream from
// the secret store into password; an inline password is masked by the API but
// still discouraged.
type Config struct {
	Host        string   `json:"host"`
	Port        port     `json:"port"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	PasswordRef string   `json:"password_ref"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	TLS         string   `json:"tls"`
}

// port accepts both 587 and "587". A channel config is raw JSON submitted by
// any client, and a quoted port is the most common way to get it wrong; a plain
// int field would answer that with "cannot unmarshal string into Go struct
// field", which tells an operator nothing about what to fix.
type port int

func (p *port) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("port %s is not a number", string(b))
	}
	*p = port(n)
	return nil
}

// New builds an smtp Notifier. Everything that can be checked without a network
// round trip is checked here, so the "Test" button and the first real delivery
// both fail with a reason instead of a timeout.
func New(raw []byte) (plugin.Notifier, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("smtp: bad config: %w", err)
		}
	}

	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		return nil, fmt.Errorf("smtp: host is required")
	}

	if c.TLS == "" {
		c.TLS = TLSStartTLS
	}
	switch c.TLS {
	case TLSStartTLS, TLSImplicit, TLSNone:
	default:
		return nil, fmt.Errorf("smtp: tls must be one of %q, %q, %q, got %q",
			TLSStartTLS, TLSImplicit, TLSNone, c.TLS)
	}

	if c.Port == 0 {
		c.Port = defaultPort(c.TLS)
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, fmt.Errorf("smtp: port %d out of range", c.Port)
	}

	from, err := mail.ParseAddress(strings.TrimSpace(c.From))
	if err != nil {
		if strings.TrimSpace(c.From) == "" {
			return nil, fmt.Errorf("smtp: from is required")
		}
		return nil, fmt.Errorf("smtp: from %q: %w", c.From, err)
	}

	if len(c.To) == 0 {
		return nil, fmt.Errorf("smtp: to is required (at least one recipient)")
	}
	to := make([]*mail.Address, 0, len(c.To))
	for _, raw := range c.To {
		addr, err := mail.ParseAddress(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("smtp: to %q: %w", raw, err)
		}
		to = append(to, addr)
	}

	if c.Username != "" {
		// An empty password next to a username almost always means the
		// password_ref went missing — say so now rather than at 2am.
		if c.Password == "" {
			return nil, fmt.Errorf("smtp: username is set but password is empty (lost password_ref?)")
		}
		// net/smtp refuses to send credentials over an unencrypted link unless
		// the server is localhost. Reporting that here beats surfacing the
		// library's bare "unencrypted connection" at delivery time.
		if c.TLS == TLSNone && !isLocalhost(c.Host) {
			return nil, fmt.Errorf("smtp: authentication over tls=%q is refused for host %q; use %q or %q",
				TLSNone, c.Host, TLSStartTLS, TLSImplicit)
		}
	}

	return &Notifier{
		cfg:  c,
		from: from,
		to:   to,
		addr: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))),
		now:  time.Now,
	}, nil
}

func defaultPort(mode string) port {
	switch mode {
	case TLSImplicit:
		return 465
	case TLSNone:
		return 25
	default:
		return 587
	}
}

// isLocalhost mirrors the check net/smtp applies before releasing a password
// over a plaintext link.
func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Notifier mails events through one SMTP server.
type Notifier struct {
	cfg  Config
	from *mail.Address
	to   []*mail.Address
	addr string
	now  func() time.Time

	// tlsConfig overrides the TLS settings used for STARTTLS and implicit TLS.
	// Production leaves it nil and gets ServerName-verified defaults; tests set
	// it so a fake server may present a self-signed certificate.
	tlsConfig *tls.Config
}

func (n *Notifier) Name() string { return "smtp" }

// Notify sends one event as one message. There is no batching: a digest that
// merges two failures into one mail would hide the second one.
func (n *Notifier) Notify(ctx context.Context, ev plugin.Event) error {
	deadline := n.now().Add(sendTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	conn, err := n.dial(ctx, deadline)
	if err != nil {
		return err
	}
	// net/smtp takes no context, so cancellation is enforced by closing the
	// connection out from under it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer conn.Close()

	c, err := netsmtp.NewClient(conn, n.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp: greeting from %s: %w", n.addr, err)
	}
	defer c.Close()

	if err := n.startTLS(c); err != nil {
		return err
	}
	if err := n.auth(c); err != nil {
		return err
	}

	if err := c.Mail(n.from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM %s: %w", n.from.Address, err)
	}
	for _, rcpt := range n.to {
		if err := c.Rcpt(rcpt.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %w", rcpt.Address, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(n.message(ev)); err != nil {
		return fmt.Errorf("smtp: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: finish message: %w", err)
	}
	// The server accepted the message when it answered end-of-DATA above. A QUIT
	// that never gets its 221 back — a relay that drops the connection, an
	// expired deadline — means the link died on the way out, not that the mail
	// was lost; returning it would log a delivered notification as failed.
	_ = c.Quit()
	return nil
}

// dial opens the transport, applying implicit TLS when configured. The deadline
// covers the entire conversation, not just the connect.
//
// It goes on the dialer too, and not only on the connection afterwards:
// delivery runs on a detached context with no deadline of its own, so a
// firewalled host that swallows the SYN would otherwise sit in connect for the
// OS timeout — minutes — with the worker slot held all the while.
func (n *Notifier) dial(ctx context.Context, deadline time.Time) (net.Conn, error) {
	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "tcp", n.addr)
	if err != nil {
		return nil, fmt.Errorf("smtp: dial %s: %w", n.addr, err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp: set deadline: %w", err)
	}
	if n.cfg.TLS != TLSImplicit {
		return conn, nil
	}
	tlsConn := tls.Client(conn, n.tlsSettings())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp: tls handshake with %s: %w", n.addr, err)
	}
	return tlsConn, nil
}

func (n *Notifier) tlsSettings() *tls.Config {
	if n.tlsConfig != nil {
		return n.tlsConfig.Clone()
	}
	return &tls.Config{ServerName: n.cfg.Host, MinVersion: tls.VersionTLS12}
}

// startTLS upgrades the connection when the mode asks for it. A server that
// does not offer STARTTLS is an error: falling back to plaintext would deliver
// the mail while quietly discarding the guarantee the operator configured.
func (n *Notifier) startTLS(c *netsmtp.Client) error {
	if n.cfg.TLS != TLSStartTLS {
		return nil
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return fmt.Errorf("smtp: %s does not offer STARTTLS; set tls=%q to send in the clear on purpose",
			n.addr, TLSNone)
	}
	if err := c.StartTLS(n.tlsSettings()); err != nil {
		return fmt.Errorf("smtp: STARTTLS with %s: %w", n.addr, err)
	}
	return nil
}

// auth authenticates when a username is configured. PLAIN is the only mechanism
// net/smtp implements; a server without it gets a message naming what it does
// advertise, so the gap is diagnosable from the delivery log alone.
func (n *Notifier) auth(c *netsmtp.Client) error {
	if n.cfg.Username == "" {
		return nil
	}
	ok, mechs := c.Extension("AUTH")
	if !ok {
		return fmt.Errorf("smtp: %s does not advertise AUTH but a username is configured", n.addr)
	}
	if !strings.Contains(strings.ToUpper(mechs), "PLAIN") {
		return fmt.Errorf("smtp: %s advertises AUTH %s; only PLAIN is supported", n.addr, mechs)
	}
	if err := c.Auth(netsmtp.PlainAuth("", n.cfg.Username, n.cfg.Password, n.cfg.Host)); err != nil {
		return fmt.Errorf("smtp: authenticate as %s: %w", n.cfg.Username, err)
	}
	return nil
}

// message renders the RFC 5322 message. Everything reaching a header goes
// through sanitizeHeader first: ev.Task is typed by an operator and ev.Message
// comes from the database engine, so both are untrusted as far as CRLF goes.
func (n *Notifier) message(ev plugin.Event) []byte {
	at := ev.At
	if at.IsZero() {
		at = n.now()
	}

	rcpts := make([]string, 0, len(n.to))
	for _, a := range n.to {
		rcpts = append(rcpts, formatAddr(a))
	}

	var b strings.Builder
	header := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(sanitizeHeader(v))
		b.WriteString("\r\n")
	}
	header("From", formatAddr(n.from))
	header("To", strings.Join(rcpts, ", "))
	header("Subject", encodeSubject(subject(ev)))
	header("Date", at.Format(time.RFC1123Z))
	header("Message-ID", messageID(n.from.Address))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	// The body is raw UTF-8; claiming 7bit would be a lie for a Cyrillic task
	// name or error text.
	header("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(body(ev, at))
	return []byte(b.String())
}

// subject puts the event kind and the task up front, so a mailbox list is
// readable without opening anything.
func subject(ev plugin.Event) string {
	return truncate(fmt.Sprintf("[duskrun] %s: %s", ev.Kind, ev.Task), subjectLimit)
}

// encodeSubject applies RFC 2047 when needed. QEncoding leaves pure ASCII
// alone, so an English subject stays readable in raw form.
func encodeSubject(s string) string {
	return mime.QEncoding.Encode("UTF-8", s)
}

// body lists one fact per line. Message goes last and its continuation lines
// are indented: mysqldump stderr is multi-line, and an unindented second line
// would read as another field.
func body(ev plugin.Event, at time.Time) string {
	var b strings.Builder
	line := func(label, value string) {
		// Normalise first: values arrive with any mix of CRLF, LF and lone CR,
		// and only then does the indent make every continuation line align.
		v := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(value)
		b.WriteString(label)
		b.WriteString(strings.ReplaceAll(v, "\n", "\r\n           "))
		b.WriteString("\r\n")
	}
	line("Событие:   ", string(ev.Kind))
	line("Задача:    ", ev.Task)
	if ev.RunID != 0 {
		// A watchdog finding or a channel test has no run behind it, and
		// "Запуск: #0" would be noise.
		line("Запуск:    ", fmt.Sprintf("#%d", ev.RunID))
	}
	line("Время:     ", at.UTC().Format("2006-01-02 15:04:05 UTC"))
	if ev.Message != "" {
		line("Сообщение: ", ev.Message)
	}
	return b.String()
}

// messageID keeps threads apart. Without it some relays score the mail as spam
// and Gmail collapses same-subject alerts into one thread, hiding tonight's
// failure under yesterday's.
func messageID(from string) string {
	domain := "duskrun.local"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		domain = d
	}
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Uniqueness degrades, delivery does not: a duplicate id is a threading
		// nuisance, a dropped alert is an outage nobody hears about.
		return fmt.Sprintf("<duskrun.%d@%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<duskrun.%s@%s>", hex.EncodeToString(buf[:]), domain)
}

// formatAddr renders an address for a header. mail.Address.String() always
// wraps in angle brackets; a bare address reads better and is what every other
// sender emits when there is no display name to carry.
func formatAddr(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.String()
}

// sanitizeHeader strips CR and LF from a header value. Without this a task
// named "x\r\nBcc: attacker@evil" would turn every notification into a mailing.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
