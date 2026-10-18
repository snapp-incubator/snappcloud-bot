package bestpractice

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status is where a report is in its life: built, waiting for a cloud admin,
// then sent, rejected or expired.
type Status string

const (
	StatusPending  Status = "pending"
	StatusSending  Status = "sending"
	StatusSent     Status = "sent"
	StatusRejected Status = "rejected"
	StatusExpired  Status = "expired"
)

// Report is one weekly scan and what became of it.
type Report struct {
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Status   Status    `json:"status"`
	Findings []Finding `json:"findings"`
	// Admins maps each team to its admins' emails, from the Team CRs.
	Admins   map[string][]string `json:"admins"`
	Scanned  []string            `json:"scanned"`
	Failed   map[string]string   `json:"failed,omitempty"`
	Ignored  map[string]int      `json:"ignored,omitempty"`
	Excluded []string            `json:"excluded,omitempty"`
	// Missing are admin emails with no Mattermost account, found at preview.
	Missing []string `json:"missing,omitempty"`
	// RootID is the preview post in the admin channel; everything about the
	// report is threaded under it.
	RootID    string    `json:"root,omitempty"`
	DecidedBy string    `json:"decidedBy,omitempty"`
	DecidedAt time.Time `json:"decidedAt,omitempty"`
	// SupersededBy names the report that replaced this one while pending.
	SupersededBy string    `json:"supersededBy,omitempty"`
	Diff         Diff      `json:"diff"`
	Delivery     *Delivery `json:"delivery,omitempty"`
}

// Diff compares a report with the one before it, over the clusters both read.
type Diff struct {
	Previous string `json:"previous,omitempty"`
	New      int    `json:"new"`
	Fixed    int    `json:"fixed"`
	// OpenLong counts findings first seen three or more weeks ago.
	OpenLong int `json:"openLong"`
}

// Delivery is what happened when an approved report was sent.
type Delivery struct {
	Sent []string `json:"sent,omitempty"`
	// NoAccount: no Mattermost user has this email.
	NoAccount []string `json:"noAccount,omitempty"`
	// AuthzFailed: the recipient's access could not be checked, so nothing
	// was sent to them (fail-closed).
	AuthzFailed []string `json:"authzFailed,omitempty"`
	PostFailed  []string `json:"postFailed,omitempty"`
	// NothingVisible: every finding for this admin was in a namespace they
	// cannot access.
	NothingVisible []string `json:"nothingVisible,omitempty"`
	// ScopeDropped counts findings withheld from a recipient for lack of access.
	ScopeDropped int `json:"scopeDropped"`
	// Resolved were in the preview but gone on the re-check; Held are new
	// since the preview, and wait for next week's approval.
	Resolved int `json:"resolved"`
	Held     int `json:"held"`
	// Unverified were sent as previewed because their cluster could not be
	// read again.
	Unverified  int  `json:"unverified"`
	Interrupted bool `json:"interrupted,omitempty"`
}

// Teams returns the teams a report has findings for, excluded ones left out.
func (r Report) Teams() []string {
	seen := map[string]bool{}
	for _, f := range r.Findings {
		if f.Team != "" && !contains(r.Excluded, f.Team) {
			seen[f.Team] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Sendable is the findings that go to teams: owned, not excluded.
func (r Report) Sendable() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Team != "" && !contains(r.Excluded, f.Team) {
			out = append(out, f)
		}
	}
	return out
}

// Intersect keeps the approved findings that the re-check still sees. A
// finding fixed since the preview is dropped; one that is new since the
// preview is held, because nobody approved it. Findings on clusters the
// re-check could not read are sent as previewed.
func Intersect(approved, current []Finding, rechecked map[string]bool) (send []Finding, resolved, held, unverified int) {
	cur := make(map[string]Finding, len(current))
	for _, f := range current {
		cur[f.Key()] = f
	}
	seen := make(map[string]bool, len(approved))
	for _, f := range approved {
		seen[f.Key()] = true
		switch c, ok := cur[f.Key()]; {
		case !rechecked[f.Cluster]:
			send = append(send, f)
			unverified++
		case ok:
			send = append(send, c)
		default:
			resolved++
		}
	}
	for k, f := range cur {
		if !seen[k] && rechecked[f.Cluster] {
			held++
		}
	}
	return send, resolved, held, unverified
}

// ErrNotFound means no report has that id.
var ErrNotFound = errors.New("no report with that id")

// keepReports bounds the history kept on disk: the latest is what commands
// act on, the one before it is what the diff compares with.
const keepReports = 6

// openLong is how long a finding stays open before the preview calls it out.
const openLong = 21 * 24 * time.Hour

// Store persists reports, the last timer slot, and when each finding was
// first seen, so a restart neither loses a pending report nor runs twice.
type Store struct {
	path        string
	expireAfter time.Duration
	mu          sync.Mutex
	st          storeState
}

type storeState struct {
	Reports   []*Report            `json:"reports"`
	LastSlot  time.Time            `json:"lastSlot"`
	FirstSeen map[string]time.Time `json:"firstSeen"`
}

// NewStore loads path ("" = memory only). expireAfter bounds how long a report
// waits for approval (default 48h).
func NewStore(path string, expireAfter time.Duration) *Store {
	if expireAfter <= 0 {
		expireAfter = 48 * time.Hour
	}
	s := &Store{path: path, expireAfter: expireAfter, st: storeState{FirstSeen: map[string]time.Time{}}}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &s.st)
		}
	}
	if s.st.FirstSeen == nil {
		s.st.FirstSeen = map[string]time.Time{}
	}
	return s
}

// Add stores a new report as pending, filling its id, expiry and diff. A
// report still pending is superseded: its data is a week old. The superseded
// one is returned so its thread can say so.
func (s *Store) Add(r *Report, now time.Time) (superseded []Report, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	r.ID = id
	r.Created = now
	r.Expires = now.Add(s.expireAfter)
	r.Status = StatusPending

	if n := len(s.st.Reports); n > 0 {
		r.Diff = diff(s.st.Reports[n-1], r)
	}
	keys := make(map[string]bool, len(r.Findings))
	for _, f := range r.Findings {
		keys[f.Key()] = true
		if _, ok := s.st.FirstSeen[f.Key()]; !ok {
			s.st.FirstSeen[f.Key()] = now
		}
		if now.Sub(s.st.FirstSeen[f.Key()]) >= openLong {
			r.Diff.OpenLong++
		}
	}
	// A cluster that was not read keeps its findings' history.
	scanned := toSet(r.Scanned)
	for k := range s.st.FirstSeen {
		if !keys[k] && scanned[clusterOfKey(k)] {
			delete(s.st.FirstSeen, k)
		}
	}

	for _, old := range s.st.Reports {
		s.expire(old, now)
		if old.Status == StatusPending {
			old.Status = StatusExpired
			old.SupersededBy = r.ID
			superseded = append(superseded, *old)
		}
	}
	s.st.Reports = append(s.st.Reports, r)
	if len(s.st.Reports) > keepReports {
		s.st.Reports = s.st.Reports[len(s.st.Reports)-keepReports:]
	}
	return superseded, s.flush()
}

func diff(prev, cur *Report) Diff {
	d := Diff{Previous: prev.ID}
	both := map[string]bool{}
	for _, c := range cur.Scanned {
		if contains(prev.Scanned, c) {
			both[c] = true
		}
	}
	keys := func(r *Report) map[string]bool {
		m := map[string]bool{}
		for _, f := range r.Findings {
			if both[f.Cluster] {
				m[f.Key()] = true
			}
		}
		return m
	}
	p, c := keys(prev), keys(cur)
	for k := range c {
		if !p[k] {
			d.New++
		}
	}
	for k := range p {
		if !c[k] {
			d.Fixed++
		}
	}
	return d
}

func clusterOfKey(k string) string {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i]
		}
	}
	return k
}

// expire moves a pending report past its expiry to expired. Caller holds mu.
func (s *Store) expire(r *Report, now time.Time) {
	if r.Status == StatusPending && now.After(r.Expires) {
		r.Status = StatusExpired
	}
}

// Get returns a report by id.
func (s *Store) Get(id string, now time.Time) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.st.Reports {
		if r.ID == id {
			s.expire(r, now)
			return *r, nil
		}
	}
	return Report{}, ErrNotFound
}

// Latest returns the newest report.
func (s *Store) Latest(now time.Time) (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.st.Reports) == 0 {
		return Report{}, false
	}
	r := s.st.Reports[len(s.st.Reports)-1]
	s.expire(r, now)
	return *r, true
}

// Update changes a report under the lock and persists it. fn's error aborts
// the change.
func (s *Store) Update(id string, now time.Time, fn func(*Report) error) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.st.Reports {
		if r.ID != id {
			continue
		}
		s.expire(r, now)
		cp := *r
		if err := fn(&cp); err != nil {
			return *r, err
		}
		*r = cp
		return cp, s.flush()
	}
	return Report{}, ErrNotFound
}

// LastSlot is the timer slot that last fired (or was skipped).
func (s *Store) LastSlot() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.LastSlot
}

// SetLastSlot records a timer slot as handled.
func (s *Store) SetLastSlot(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastSlot = t
	return s.flush()
}

// flush writes the state. Writes are rare (a few per week), so every change is
// written at once rather than on a timer. Caller holds mu.
func (s *Store) flush() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// idAlphabet leaves out the characters people misread when they type an id
// back, as schedule ids do.
const idAlphabet = "abcdefghjkmnpqrstwxyz23456789"

// newID returns a short random id. Caller holds mu.
func (s *Store) newID() (string, error) {
	buf := make([]byte, 5)
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate report id: %w", err)
		}
		id := make([]byte, len(buf))
		for i, b := range buf {
			id[i] = idAlphabet[int(b)%len(idAlphabet)]
		}
		taken := false
		for _, r := range s.st.Reports {
			taken = taken || r.ID == string(id)
		}
		if !taken {
			return string(id), nil
		}
	}
	return "", errors.New("could not generate a unique report id")
}
