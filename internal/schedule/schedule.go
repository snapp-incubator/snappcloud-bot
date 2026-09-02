// Package schedule stores and fires user-defined recurring queries.
//
// A schedule is a saved question plus a cadence. When it fires, the bot runs the
// query exactly as if the user had asked it, and posts the answer where the
// schedule was created. Authorization is NOT stored: the user's scope is
// resolved fresh on every run, so a schedule can never outlive the access it
// was created with.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one user's recurring query.
type Entry struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`    // SSO identity that owns it
	ChannelID string    `json:"channel"` // where the answer is posted
	RootID    string    `json:"root"`    // thread root ("" for a DM)
	Query     string    `json:"query"`   // the question to run
	Spec      string    `json:"spec"`    // human-readable cadence, e.g. "every day at 09:00"
	Every     Duration  `json:"every"`   // interval between runs
	At        string    `json:"at"`      // "HH:MM" for daily schedules ("" otherwise)
	Next      time.Time `json:"next"`    // next fire time
	Created   time.Time `json:"created"`
	// Failures counts consecutive failed runs; a schedule that keeps failing is
	// disabled rather than retried forever.
	Failures int `json:"failures"`
}

// Duration is a time.Duration that serializes as a string.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Limits bound what users may schedule, so recurring work cannot grow without
// ceiling: every entry costs an LLM run plus MCP calls each time it fires.
type Limits struct {
	// PerUser caps schedules per user (default 5).
	PerUser int
	// Total caps schedules across all users (default 200).
	Total int
	// MinInterval is the shortest allowed cadence (default 1h).
	MinInterval time.Duration
	// MaxFailures disables a schedule after this many consecutive failures
	// (default 5).
	MaxFailures int
}

func (l *Limits) applyDefaults() {
	if l.PerUser <= 0 {
		l.PerUser = 5
	}
	if l.Total <= 0 {
		l.Total = 200
	}
	if l.MinInterval <= 0 {
		l.MinInterval = time.Hour
	}
	if l.MaxFailures <= 0 {
		l.MaxFailures = 5
	}
}

var (
	// ErrTooManyForUser means the caller is at their schedule limit.
	ErrTooManyForUser = errors.New("you already have the maximum number of schedules")
	// ErrTooManyTotal means the bot is at its global schedule limit.
	ErrTooManyTotal = errors.New("the bot is at its global schedule limit")
	// ErrTooFrequent means the requested cadence is below MinInterval.
	ErrTooFrequent = errors.New("that is too frequent")
	// ErrNotFound means no such schedule for this user.
	ErrNotFound = errors.New("no schedule with that id")
)

// Store holds schedules, persisted to a file so they survive restarts.
type Store struct {
	path   string
	limits Limits
	mu     sync.Mutex
	m      map[string]*Entry
	dirty  bool
	nextID int
}

// NewStore loads any persisted schedules from path ("" = memory only).
func NewStore(path string, limits Limits) *Store {
	limits.applyDefaults()
	s := &Store{path: path, limits: limits, m: map[string]*Entry{}}
	s.load()
	return s
}

// Limits returns the configured bounds (for user-facing messages).
func (s *Store) Limits() Limits { return s.limits }

// Add stores a new schedule for user, enforcing the limits.
func (s *Store) Add(e *Entry) error {
	if time.Duration(e.Every) < s.limits.MinInterval {
		return fmt.Errorf("%w: the minimum is every %s", ErrTooFrequent, human(s.limits.MinInterval))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= s.limits.Total {
		return ErrTooManyTotal
	}
	mine := 0
	for _, x := range s.m {
		if x.User == e.User {
			mine++
		}
	}
	if mine >= s.limits.PerUser {
		return fmt.Errorf("%w (%d)", ErrTooManyForUser, s.limits.PerUser)
	}
	s.nextID++
	e.ID = fmt.Sprintf("%d", s.nextID)
	e.Created = time.Now()
	s.m[e.ID] = e
	s.dirty = true
	return nil
}

// List returns a user's schedules, oldest first.
func (s *Store) List(user string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.m {
		if e.User == user {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Delete removes one of the user's schedules. A user can only delete their own.
func (s *Store) Delete(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok || e.User != user {
		return ErrNotFound
	}
	delete(s.m, id)
	s.dirty = true
	return nil
}

// Due returns the schedules whose next run time has passed, and advances each
// one to its following slot so a slow run cannot fire twice.
func (s *Store) Due(now time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.m {
		if now.Before(e.Next) {
			continue
		}
		out = append(out, *e)
		e.Next = advance(e, now)
		s.dirty = true
	}
	return out
}

// RecordResult resets or increments the failure count, disabling (removing) a
// schedule that keeps failing. Returns true when the schedule was removed.
func (s *Store) RecordResult(id string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return false
	}
	if err == nil {
		if e.Failures != 0 {
			e.Failures = 0
			s.dirty = true
		}
		return false
	}
	e.Failures++
	s.dirty = true
	if e.Failures >= s.limits.MaxFailures {
		delete(s.m, id)
		return true
	}
	return false
}

// Count returns the number of stored schedules.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// advance moves an entry to its next slot strictly after now.
func advance(e *Entry, now time.Time) time.Time {
	every := time.Duration(e.Every)
	next := e.Next
	if next.IsZero() {
		next = now
	}
	for !next.After(now) {
		next = next.Add(every)
	}
	return next
}

// --- persistence -----------------------------------------------------------

func (s *Store) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // first run
	}
	var list []Entry
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for i := range list {
		e := list[i]
		s.m[e.ID] = &e
		if n := atoi(e.ID); n > s.nextID {
			s.nextID = n
		}
	}
}

// Flush writes the schedules to disk if they changed.
func (s *Store) Flush() {
	if s.path == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	list := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		list = append(list, *e)
	}
	s.dirty = false
	s.mu.Unlock()

	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	_ = os.Rename(tmp, s.path)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func human(d time.Duration) string {
	s := d.String()
	return strings.TrimSuffix(s, "0s")
}
