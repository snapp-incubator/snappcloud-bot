package botoauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

var ErrUnlinked = errors.New("pardis account needs linking")

type pending struct {
	User, Email, Verifier string
	Expires               time.Time
}
type Links struct {
	pendingMu sync.Mutex
	accounts  [64]sync.Mutex
	pending   map[string]pending
	cfg       Outgoing
	oauth     oauth2.Config
	cipher    cipher.AEAD
	verify    func(context.Context, string, string) error
}

func NewLinks(cfg Outgoing, secret, key string, verify func(context.Context, string, string) error) (*Links, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("PARDIS_OAUTH_STORAGE_KEY must encode 32 bytes in base64")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cfg.StoreDir, 0700); err != nil {
		return nil, err
	}
	return &Links{
		cfg: cfg, cipher: aead, pending: make(map[string]pending), verify: verify,
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: secret, RedirectURL: cfg.CallbackURL,
			Endpoint: oauth2.Endpoint{AuthURL: cfg.AuthorizeURL, TokenURL: cfg.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
			Scopes:   []string{"mcp:tools"},
		},
	}, nil
}

// Serialize refresh, callback and disconnect for each account without holding
// the pending-state lock during network calls. Stripes bound memory usage.
func (l *Links) accountLock(user string) *sync.Mutex {
	sum := sha256.Sum256([]byte(user))
	return &l.accounts[int(sum[0])%len(l.accounts)]
}

func (l *Links) cancel(user string) {
	for state, p := range l.pending {
		if p.User == user || time.Now().After(p.Expires) {
			delete(l.pending, state)
		}
	}
}
func (l *Links) Start(user, email string) string {
	lock := l.accountLock(user)
	lock.Lock()
	defer lock.Unlock()
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.cancel(user)
	state := rand.Text()
	verifier := oauth2.GenerateVerifier()
	l.pending[state] = pending{user, email, verifier, time.Now().Add(5 * time.Minute)}
	return l.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", l.cfg.Resource))
}
func (l *Links) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	state := q.Get("state")
	l.pendingMu.Lock()
	p, ok := l.pending[state]
	l.pendingMu.Unlock()
	if !ok {
		http.Error(w, "Login expired or declined. Start again in your bot DM.", http.StatusBadRequest)
		return
	}
	lock := l.accountLock(p.User)
	lock.Lock()
	defer lock.Unlock()
	l.pendingMu.Lock()
	p, ok = l.pending[state]
	delete(l.pending, state)
	l.pendingMu.Unlock()
	if !ok || time.Now().After(p.Expires) || q.Get("code") == "" {
		http.Error(w, "Login expired or declined. Start again in your bot DM.", http.StatusBadRequest)
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, client)
	t, err := l.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier), oauth2.SetAuthURLParam("resource", l.cfg.Resource))
	if err != nil || !t.Valid() || t.Expiry.IsZero() || l.verify(ctx, t.AccessToken, p.Email) != nil {
		http.Error(w, "Could not link this account. Sign in with your Mattermost work identity.", http.StatusForbidden)
		return
	}
	if err = l.save(p.User, t); err != nil {
		http.Error(w, "Could not save session.", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("Pardis connected. Return to your bot DM."))
}
func (l *Links) file(user string) string {
	h := sha256.Sum256([]byte(user))
	return filepath.Join(l.cfg.StoreDir, hex.EncodeToString(h[:]))
}
func (l *Links) save(user string, t *oauth2.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	b = l.cipher.Seal(nil, nil, b, []byte(user))
	f, err := os.CreateTemp(l.cfg.StoreDir, ".session-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), l.file(user))
}

// Disconnect revokes the server grant before forgetting it. On a transient
// failure the caller can retry revocation using the same saved credential.
func (l *Links) Disconnect(ctx context.Context, user string) error {
	lock := l.accountLock(user)
	lock.Lock()
	defer lock.Unlock()
	l.pendingMu.Lock()
	l.cancel(user)
	l.pendingMu.Unlock()
	t, err := l.load(user)
	if errors.Is(err, ErrUnlinked) {
		return nil
	}
	if err != nil {
		return err
	}
	token, hint := t.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = t.AccessToken, "access_token"
	}
	form := url.Values{"token": {token}, "token_type_hint": {hint}, "client_id": {l.cfg.ClientID}}
	if l.oauth.ClientSecret != "" {
		form.Set("client_secret", l.oauth.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("pardis revocation unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("pardis revocation unavailable")
	}
	return os.Remove(l.file(user))
}
func (l *Links) load(user string) (*oauth2.Token, error) {
	b, err := os.ReadFile(l.file(user))
	if os.IsNotExist(err) {
		return nil, ErrUnlinked
	}
	if err != nil {
		return nil, err
	}
	b, err = l.cipher.Open(nil, nil, b, []byte(user))
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err = json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
func (l *Links) Token(ctx context.Context, user string) (string, error) {
	lock := l.accountLock(user)
	lock.Lock()
	defer lock.Unlock()
	t, err := l.load(user)
	if err != nil {
		return "", err
	}
	if t.Valid() {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", ErrUnlinked
	}
	refreshed, err := l.oauth.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), t).Token()
	if err != nil {
		var e *oauth2.RetrieveError
		if errors.As(err, &e) && e.ErrorCode == "invalid_grant" {
			return "", ErrUnlinked
		}
		return "", errors.New("pardis token refresh unavailable")
	}
	if !refreshed.Valid() || refreshed.Expiry.IsZero() {
		return "", ErrUnlinked
	}
	if err = l.save(user, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}
