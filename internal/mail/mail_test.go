package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	netmail "net/mail"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a plaintext SMTP server that records one session's commands
// and the message. It never offers STARTTLS; it refuses any recipient whose
// address starts with "nobody".
type fakeSMTP struct {
	ln   net.Listener
	auth bool // advertise AUTH PLAIN LOGIN

	mu       sync.Mutex
	commands []string
	data     string
}

func newFakeSMTP(t *testing.T, auth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, auth: auth}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.session(conn)
	}
}

func (f *fakeSMTP) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	say := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch {
		case verb == "EHLO":
			if f.auth {
				say("250-fake")
				say("250 AUTH PLAIN LOGIN")
			} else {
				say("250 fake")
			}
		case verb == "RCPT" && strings.Contains(strings.ToLower(line), "<nobody"):
			say("550 5.1.1 no such user")
		case verb == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case verb == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) seen() ([]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), f.data
}

func newSender(t *testing.T, f *fakeSMTP, user string) *Sender {
	t.Helper()
	s, err := New(Config{Host: "127.0.0.1", Port: f.port(), Username: user, Password: "s3cret",
		From: "SnappCloud <bot@snapp.cab>", ReplyTo: "cloud@snapp.cab", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSendBuildsAMultipartMessage(t *testing.T) {
	f := newFakeSMTP(t, false)
	s := newSender(t, f, "")
	rejected, err := s.Send(context.Background(), Message{
		To: []string{"alice@snapp.cab", "bob@snapp.cab"}, Cc: []string{"cto@snapp.cab", "Alice@snapp.cab"},
		Subject:     "Best-practice check — 2 findings 🔴",
		Text:        "PDB `api` allows no evictions\nFix: set maxUnavailable: 1",
		HTML:        "<p>PDB <code>api</code> allows no evictions</p>",
		Attachments: []Attachment{{Name: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 fake")}},
	})
	if err != nil || len(rejected) != 0 {
		t.Fatal(rejected, err)
	}
	cmds, data := f.seen()
	var rcpts []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "RCPT TO:") {
			rcpts = append(rcpts, c)
		}
	}
	// alice is in To already: Cc does not deliver her a second copy.
	if !slices.Contains(cmds, "MAIL FROM:<bot@snapp.cab>") ||
		!slices.Equal(rcpts, []string{"RCPT TO:<alice@snapp.cab>", "RCPT TO:<bob@snapp.cab>", "RCPT TO:<cto@snapp.cab>"}) {
		t.Errorf("envelope = %v", cmds)
	}

	msg, err := netmail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != "Best-practice check — 2 findings 🔴" {
		t.Errorf("subject = %q", subject)
	}
	for k, want := range map[string]string{"To": "<alice@snapp.cab>, <bob@snapp.cab>", "Cc": "<cto@snapp.cab>",
		"Reply-To": "<cloud@snapp.cab>", "Auto-Submitted": "auto-generated", "From": `"SnappCloud" <bot@snapp.cab>`} {
		if got := msg.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	mixed := parts(t, msg.Header.Get("Content-Type"), msg.Body)
	if len(mixed) != 2 || !strings.HasPrefix(mixed[0].typ, "multipart/alternative") {
		t.Fatalf("mixed parts = %v", mixed)
	}
	alt := parts(t, mixed[0].typ, strings.NewReader(mixed[0].body))
	if len(alt) != 2 || !strings.HasPrefix(alt[0].typ, "text/plain") || !strings.HasPrefix(alt[1].typ, "text/html") {
		t.Fatalf("alternative parts = %v", alt)
	}
	if !strings.Contains(alt[0].body, "Fix: set maxUnavailable: 1") || !strings.Contains(alt[1].body, "<code>api</code>") {
		t.Errorf("bodies = %v", alt)
	}
	pdf := mixed[1]
	if !strings.HasPrefix(pdf.typ, "application/pdf") || pdf.filename != "report.pdf" {
		t.Errorf("attachment = %s %q", pdf.typ, pdf.filename)
	}
	if got, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(pdf.body, "\r\n", "")); string(got) != "%PDF-1.4 fake" {
		t.Errorf("attachment body = %q", got)
	}
}

type part struct{ typ, filename, body string }

// parts reads one multipart level. Quoted-printable parts come back decoded;
// base64 ones as sent.
func parts(t *testing.T, contentType string, r io.Reader) []part {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(r, params["boundary"])
	var out []part
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		out = append(out, part{p.Header.Get("Content-Type"), p.FileName(), string(b)})
	}
}

func TestSendReportsARejectedRecipient(t *testing.T) {
	f := newFakeSMTP(t, false)
	rejected, err := newSender(t, f, "").Send(context.Background(),
		Message{To: []string{"nobody@snapp.cab"}, Subject: "x", Text: "x", HTML: "x"})
	if !errors.Is(err, ErrRejected) || !slices.Equal(rejected, []string{"nobody@snapp.cab"}) {
		t.Fatalf("rejected = %v, err = %v, want ErrRejected", rejected, err)
	}
	if _, data := f.seen(); data != "" {
		t.Error("message sent to a rejected recipient")
	}
}

// One bad address in a Team CR must not keep the email from everyone else.
func TestSendSkipsTheRecipientsItCannotReach(t *testing.T) {
	f := newFakeSMTP(t, false)
	rejected, err := newSender(t, f, "").Send(context.Background(), Message{
		To: []string{"nobody@snapp.cab", "not an address", "alice@snapp.cab"}, Subject: "x", Text: "x", HTML: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rejected, []string{"not an address", "nobody@snapp.cab"}) {
		t.Errorf("rejected = %v", rejected)
	}
	if _, data := f.seen(); !strings.Contains(data, "To: <nobody@snapp.cab>, <alice@snapp.cab>") {
		t.Errorf("not sent to alice: %q", data)
	}
}

// A server that offers AUTH but not STARTTLS would receive the password in
// clear: the sender must stop before AUTH.
func TestSendNeverSendsThePasswordInClear(t *testing.T) {
	f := newFakeSMTP(t, true)
	_, err := newSender(t, f, "bot@snapp.cab").Send(context.Background(),
		Message{To: []string{"alice@snapp.cab"}, Subject: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v", err)
	}
	cmds, _ := f.seen()
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), "AUTH") {
			t.Fatalf("AUTH sent over plaintext: %v", cmds)
		}
	}
}

func TestNewValidates(t *testing.T) {
	for _, c := range []Config{
		{From: "bot@snapp.cab"},
		{Host: "mail.snapp.cab", From: "not an address"},
		{Host: "mail.snapp.cab", From: "bot@snapp.cab", ReplyTo: "nope"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
	s, err := New(Config{Host: "mail.snapp.cab", From: "bot@snapp.cab"})
	if err != nil || s.cfg.Port != 587 || s.CanReply() {
		t.Errorf("defaults: %+v, %v", s, err)
	}
}

func TestSendRefusesAHeaderInTheAddress(t *testing.T) {
	f := newFakeSMTP(t, false)
	_, err := newSender(t, f, "").Send(context.Background(),
		Message{To: []string{"a@snapp.cab\r\nBcc: x@evil.com"}, Subject: "x"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v", err)
	}
	if cmds, _ := f.seen(); len(cmds) != 0 {
		t.Errorf("connected for an invalid address: %v", cmds)
	}
}

// Exchange (mail.snapp.cab) offers only GSSAPI, NTLM and, after STARTTLS,
// LOGIN: the sender must answer its two prompts.
func TestLoginAuthAnswersExchangePrompts(t *testing.T) {
	a := loginAuth{"bot@snapp.cab", "s3cret"}
	if mech, resp, err := a.Start(nil); mech != "LOGIN" || resp != nil || err != nil {
		t.Fatalf("start = %q %q %v", mech, resp, err)
	}
	for challenge, want := range map[string]string{"Username:": "bot@snapp.cab", "Password:": "s3cret"} {
		if got, err := a.Next([]byte(challenge), true); err != nil || string(got) != want {
			t.Errorf("%s -> %q, %v", challenge, got, err)
		}
	}
	if _, err := a.Next([]byte("Token:"), true); err == nil {
		t.Error("an unknown prompt must fail, not leak the password")
	}
}
