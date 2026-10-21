// Package mail sends email through an SMTP server: one message, one
// connection. It is used for the best-practice report, which goes to people
// by email rather than by Mattermost.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Message is one email with a plain-text and an HTML body, and attachments.
type Message struct {
	To          []string
	Cc          []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
}

// Attachment is a file sent with a message.
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// ErrRejected marks a message no recipient could be sent: the server refused
// every address permanently (a 5xx reply to RCPT TO), or none was valid.
var ErrRejected = errors.New("every recipient rejected")

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

// Send delivers one message to every address in To and Cc that is valid and
// that the server accepts. It returns the addresses it skipped: an invalid
// one, or one the server refused permanently. When that is all of them
// nothing is sent and the error is ErrRejected. The password is only ever
// sent over TLS.
func (s *Sender) Send(ctx context.Context, m Message) (rejected []string, err error) {
	seen := map[string]bool{}
	to, badTo := parseList(m.To, seen)
	cc, badCc := parseList(m.Cc, seen)
	rejected = append(badTo, badCc...)
	if len(to)+len(cc) == 0 {
		return rejected, fmt.Errorf("%w: no valid address in %q", ErrRejected, append(m.To, m.Cc...))
	}
	body, err := s.build(to, cc, m)
	if err != nil {
		return rejected, err
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
		return rejected, fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// SMTP calls take no context: a deadline, and closing on cancel, bound them.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return rejected, fmt.Errorf("mail: %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	_, secure := conn.(*tls.Conn)
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return rejected, fmt.Errorf("mail: starttls: %w", err)
			}
			secure = true
		}
	}
	if s.cfg.Username != "" {
		if !secure {
			return rejected, fmt.Errorf("mail: %s offers no STARTTLS; refusing to send the password in clear (port 465 uses TLS)", addr)
		}
		auth, err := s.auth(c)
		if err != nil {
			return rejected, err
		}
		if err := c.Auth(auth); err != nil {
			return rejected, fmt.Errorf("mail: auth as %s: %w", s.cfg.Username, err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return rejected, fmt.Errorf("mail: MAIL FROM %s: %w", s.from.Address, err)
	}
	accepted := 0
	for _, a := range append(to, cc...) {
		if err := c.Rcpt(a.Address); err != nil {
			var te *textproto.Error
			if errors.As(err, &te) && te.Code >= 500 {
				rejected = append(rejected, a.Address)
				continue
			}
			return rejected, fmt.Errorf("mail: RCPT TO %s: %w", a.Address, err)
		}
		accepted++
	}
	if accepted == 0 {
		return rejected, fmt.Errorf("%w: %s", ErrRejected, strings.Join(rejected, ", "))
	}
	w, err := c.Data()
	if err != nil {
		return rejected, fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return rejected, fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return rejected, fmt.Errorf("mail: send: %w", err)
	}
	// The server has accepted the message; a failed QUIT does not undo that.
	_ = c.Quit()
	return rejected, nil
}

// parseList parses each address on its own, so one bad address (say, a typo
// in a Team CR) does not stop the message reaching the rest. An address
// already in seen is dropped, so nobody gets two copies.
func parseList(raw []string, seen map[string]bool) (ok []*netmail.Address, bad []string) {
	for _, r := range raw {
		a, err := netmail.ParseAddress(r)
		if err != nil {
			bad = append(bad, r)
			continue
		}
		if k := strings.ToLower(a.Address); !seen[k] {
			seen[k] = true
			ok = append(ok, a)
		}
	}
	return ok, bad
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
// show. With attachments, that body is the first part of a multipart/mixed.
func (s *Sender) build(to, cc []*netmail.Address, m Message) ([]byte, error) {
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", s.from.String())
	if len(to) > 0 {
		h("To", joinAddrs(to))
	}
	if len(cc) > 0 {
		h("Cc", joinAddrs(cc))
	}
	if s.replyTo != nil {
		h("Reply-To", s.replyTo.String())
	}
	h("Subject", mime.QEncoding.Encode("utf-8", strings.Join(strings.Fields(m.Subject), " ")))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", messageID(s.from.Address))
	h("MIME-Version", "1.0")
	// RFC 3834: out-of-office replies and the like are not sent back.
	h("Auto-Submitted", "auto-generated")

	if len(m.Attachments) == 0 {
		alt := multipart.NewWriter(&b)
		h("Content-Type", `multipart/alternative; boundary="`+alt.Boundary()+`"`)
		b.WriteString("\r\n")
		return b.Bytes(), writeAlternative(alt, m)
	}

	mixed := multipart.NewWriter(&b)
	h("Content-Type", `multipart/mixed; boundary="`+mixed.Boundary()+`"`)
	b.WriteString("\r\n")
	var altBody bytes.Buffer
	alt := multipart.NewWriter(&altBody)
	if err := writeAlternative(alt, m); err != nil {
		return nil, err
	}
	pw, err := mixed.CreatePart(textproto.MIMEHeader{
		"Content-Type": {`multipart/alternative; boundary="` + alt.Boundary() + `"`},
	})
	if err != nil {
		return nil, err
	}
	if _, err := pw.Write(altBody.Bytes()); err != nil {
		return nil, err
	}
	for _, a := range m.Attachments {
		typ := a.ContentType
		if typ == "" {
			typ = "application/octet-stream"
		}
		pw, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(typ, map[string]string{"name": a.Name})},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": a.Name})},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeBase64(pw, a.Data); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeAlternative(mw *multipart.Writer, m Message) error {
	for _, part := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return err
		}
		qw := quotedprintable.NewWriter(pw)
		if _, err := qw.Write([]byte(part.body)); err != nil {
			return err
		}
		if err := qw.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}

// writeBase64 writes data in lines of 76 characters (RFC 2045).
func writeBase64(w io.Writer, data []byte) error {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 0 {
		n := min(76, len(enc))
		if _, err := io.WriteString(w, enc[:n]+"\r\n"); err != nil {
			return err
		}
		enc = enc[n:]
	}
	return nil
}

func joinAddrs(as []*netmail.Address) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
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
