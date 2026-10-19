package mail

import (
	"bufio"
	"context"
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
	err := s.Send(context.Background(), Message{
		To: "alice@snapp.cab", Subject: "Best-practice check — 2 findings 🔴",
		Text: "PDB `api` allows no evictions\nFix: set maxUnavailable: 1",
		HTML: "<p>PDB <code>api</code> allows no evictions</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmds, data := f.seen()
	if !slices.Contains(cmds, "MAIL FROM:<bot@snapp.cab>") || !slices.Contains(cmds, "RCPT TO:<alice@snapp.cab>") {
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
	for k, want := range map[string]string{"To": "<alice@snapp.cab>", "Reply-To": "<cloud@snapp.cab>",
		"Auto-Submitted": "auto-generated", "From": `"SnappCloud" <bot@snapp.cab>`} {
		if got := msg.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types, bodies []string
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		types = append(types, p.Header.Get("Content-Type"))
		bodies = append(bodies, string(b))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("parts = %v", types)
	}
	if !strings.Contains(bodies[0], "Fix: set maxUnavailable: 1") || !strings.Contains(bodies[1], "<code>api</code>") {
		t.Errorf("bodies = %q", bodies)
	}
}

func TestSendReportsARejectedRecipient(t *testing.T) {
	f := newFakeSMTP(t, false)
	err := newSender(t, f, "").Send(context.Background(), Message{To: "nobody@snapp.cab", Subject: "x", Text: "x", HTML: "x"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if _, data := f.seen(); data != "" {
		t.Error("message sent to a rejected recipient")
	}
}

// A server that offers AUTH but not STARTTLS would receive the password in
// clear: the sender must stop before AUTH.
func TestSendNeverSendsThePasswordInClear(t *testing.T) {
	f := newFakeSMTP(t, true)
	err := newSender(t, f, "bot@snapp.cab").Send(context.Background(), Message{To: "alice@snapp.cab", Subject: "x"})
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
	err := newSender(t, f, "").Send(context.Background(), Message{To: "a@snapp.cab\r\nBcc: x@evil.com", Subject: "x"})
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
