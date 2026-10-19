// Package mail sends email through an SMTP server: one message, one
// connection. It is used for the best-practice report, which goes to people
// by email rather than by Mattermost.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is one SMTP server and the identity mail is sent as.
type Config struct {
	Host string
	// Port is 587 by default. 465 means TLS from the first byte; on any other
	// port the connection is upgraded with STARTTLS when the server offers it.
	Port     int
	Username string
	Password string
	// From is the sender, e.g. "SnappCloud <snappcloud-bot@snapp.cab>".
	From string
	// ReplyTo is where replies go, when not to From.
	ReplyTo string
	// Timeout bounds one message, connection to QUIT (default 30s).
	Timeout time.Duration
}

// Message is one email with a plain-text and an HTML body.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// ErrRejected marks a recipient the server refused permanently (a 5xx reply
// to RCPT TO): usually no mailbox has that address.
var ErrRejected = errors.New("recipient rejected")

// Sender sends mail through one SMTP server.
type Sender struct {
	cfg     Config
	from    *netmail.Address
	replyTo *netmail.Address
}

// New checks the configuration. Nothing is dialled until the first Send.
func New(cfg Config) (*Sender, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("mail: host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	s := &Sender{cfg: cfg}
	var err error
	if s.from, err = netmail.ParseAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("mail: from %q: %w", cfg.From, err)
	}
	if cfg.ReplyTo != "" {
		if s.replyTo, err = netmail.ParseAddress(cfg.ReplyTo); err != nil {
			return nil, fmt.Errorf("mail: replyTo %q: %w", cfg.ReplyTo, err)
		}
	}
	return s, nil
}

// CanReply reports whether a reply reaches someone: a Reply-To is set.
func (s *Sender) CanReply() bool { return s.replyTo != nil }

// Send delivers one message. The password is only ever sent over TLS.
func (s *Sender) Send(ctx context.Context, m Message) error {
	to, err := netmail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("%w: %q is not an address: %w", ErrRejected, m.To, err)
	}
	body, err := s.build(to, m)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	tlsConf := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if s.cfg.Port == 465 {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// SMTP calls take no context: a deadline, and closing on cancel, bound them.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	_, secure := conn.(*tls.Conn)
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("mail: starttls: %w", err)
			}
			secure = true
		}
	}
	if s.cfg.Username != "" {
		if !secure {
			return fmt.Errorf("mail: %s offers no STARTTLS; refusing to send the password in clear (port 465 uses TLS)", addr)
		}
		auth, err := s.auth(c)
		if err != nil {
			return err
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mail: auth as %s: %w", s.cfg.Username, err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("mail: MAIL FROM %s: %w", s.from.Address, err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 {
			return fmt.Errorf("%w: %s: %w", ErrRejected, to.Address, err)
		}
		return fmt.Errorf("mail: RCPT TO %s: %w", to.Address, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	// The server has accepted the message; a failed QUIT does not undo that.
	_ = c.Quit()
	return nil
}

// auth picks PLAIN, or LOGIN (Exchange often offers only that).
func (s *Sender) auth(c *smtp.Client) (smtp.Auth, error) {
	ok, params := c.Extension("AUTH")
	mechs := strings.Fields(strings.ToUpper(params))
	switch {
	case !ok:
		return nil, errors.New("mail: a username is configured but the server offers no AUTH")
	case slices.Contains(mechs, "PLAIN"):
		return smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host), nil
	case slices.Contains(mechs, "LOGIN"):
		return loginAuth{s.cfg.Username, s.cfg.Password}, nil
	}
	return nil, fmt.Errorf("mail: the server offers AUTH %s; only PLAIN and LOGIN are supported", params)
}

type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("mail: unexpected LOGIN challenge %q", challenge)
}

// build renders the message: headers, then a multipart/alternative body with
// the text first and the HTML last, as clients prefer the last part they can
// show.
func (s *Sender) build(to *netmail.Address, m Message) ([]byte, error) {
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", s.from.String())
	h("To", to.String())
	if s.replyTo != nil {
		h("Reply-To", s.replyTo.String())
	}
	h("Subject", mime.QEncoding.Encode("utf-8", strings.Join(strings.Fields(m.Subject), " ")))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", messageID(s.from.Address))
	h("MIME-Version", "1.0")
	// RFC 3834: out-of-office replies and the like are not sent back.
	h("Auto-Submitted", "auto-generated")

	mw := multipart.NewWriter(&b)
	h("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	for _, part := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qw := quotedprintable.NewWriter(pw)
		if _, err := qw.Write([]byte(part.body)); err != nil {
			return nil, err
		}
		if err := qw.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func messageID(from string) string {
	host := "snappcloud-bot"
	if _, d, ok := strings.Cut(from, "@"); ok {
		host = d
	}
	var r [12]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(r[:]), host)
}
