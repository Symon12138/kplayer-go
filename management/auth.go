package management

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrInvalidCredentials is returned by AuthService.Login for every failed
// authentication: an unknown user, a disabled user and a wrong password all
// produce the same error, so the API does not leak which users exist
// (username enumeration).
var ErrInvalidCredentials = errors.New("management: invalid credentials")

// Session is one authenticated login: a bearer token (the session ID)
// bound to a user. Username is a redundant snapshot of the user's name at
// login time, so sessions can be listed and displayed without joining
// against the user collection. ExpiresAt is the moment the token stops
// being valid; after that the session is treated as gone.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// defaultMaxSessions caps the total number of sessions kept in the store,
// guarding against unbounded growth from never-logging-out clients.
const defaultMaxSessions = 1000

// SessionService provides create/validate/delete over the sessions of a
// Store. Sessions expire after their TTL and are dropped lazily (on
// Validate) and periodically (PurgeExpired). The collection is capped at
// maxSessions: creating past the cap evicts the oldest sessions, so a
// flood of logins cannot grow the store without bound.
type SessionService struct {
	store       *Store
	maxSessions int
}

// SessionServiceOption configures a SessionService.
type SessionServiceOption func(*SessionService)

// WithMaxSessions caps the total number of sessions kept in the store.
// When a Create would push the collection past the cap, the oldest
// sessions (by CreatedAt, the ID breaking ties) are evicted in the same
// update, so the newest maxSessions survive. Defaults to 1000; a
// non-positive value keeps the collection unbounded.
func WithMaxSessions(n int) SessionServiceOption {
	return func(ss *SessionService) { ss.maxSessions = n }
}

// NewSessionService returns a SessionService backed by store.
func NewSessionService(store *Store, opts ...SessionServiceOption) *SessionService {
	ss := &SessionService{store: store, maxSessions: defaultMaxSessions}
	for _, opt := range opts {
		opt(ss)
	}
	return ss
}

// Create opens a new session for the user identified by userID, storing a
// username snapshot for convenient listing. ttl must be positive
// (ErrInvalid). The token is 32 random bytes hex-encoded (64 characters)
// and doubles as the session ID. When the store already holds
// maxSessions sessions, the oldest ones are evicted in the same update.
func (ss *SessionService) Create(userID, username string, ttl time.Duration) (*Session, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("session: %w: ttl must be positive", ErrInvalid)
	}
	now := time.Now()
	s := &Session{
		ID:        newToken(),
		UserID:    userID,
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	err := ss.store.Update(func(d *Data) error {
		d.Sessions = append(d.Sessions, s)
		d.Sessions = ss.evict(d.Sessions)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Validate returns the session for token if it exists and has not expired.
// An expired session is removed (persisted) and reported as not found; if
// a concurrent delete already removed it the removal update is a no-op. An
// empty token is rejected with ErrInvalid.
func (ss *SessionService) Validate(token string) (*Session, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("session: %w: empty token", ErrInvalid)
	}
	now := time.Now()
	var found *Session
	ss.store.View(func(d *Data) {
		for _, s := range d.Sessions {
			if s.ID == token {
				found = s
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("session %q: %w", token, ErrNotFound)
	}
	if !sessionExpired(found, now) {
		return found, nil
	}
	// Expired: drop it and report not found. Tokens are random, so an
	// expired session can never have been recreated under the same ID.
	_ = ss.store.Update(func(d *Data) error {
		for i, s := range d.Sessions {
			if s.ID == token {
				d.Sessions = append(d.Sessions[:i], d.Sessions[i+1:]...)
				return nil
			}
		}
		return errNoop
	})
	return nil, fmt.Errorf("session %q: %w", token, ErrNotFound)
}

// Delete removes the session with the given token (logout). A token that
// does not exist is reported with ErrNotFound; an empty token is rejected
// with ErrInvalid.
func (ss *SessionService) Delete(token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("session: %w: empty token", ErrInvalid)
	}
	return ss.store.Update(func(d *Data) error {
		for i, s := range d.Sessions {
			if s.ID == token {
				d.Sessions = append(d.Sessions[:i], d.Sessions[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("session %q: %w", token, ErrNotFound)
	})
}

// PurgeExpired removes every session expired at or before now and returns
// the number removed. The now parameter keeps the method deterministic
// for tests. When nothing is removed it is a no-op: the store is not
// rewritten and root UpdatedAt is not bumped.
func (ss *SessionService) PurgeExpired(now time.Time) (int, error) {
	removed := 0
	err := ss.store.Update(func(d *Data) error {
		kept := d.Sessions[:0]
		for _, s := range d.Sessions {
			if sessionExpired(s, now) {
				removed++
				continue
			}
			kept = append(kept, s)
		}
		if removed == 0 {
			return errNoop
		}
		d.Sessions = kept
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// evict trims sessions to the newest maxSessions when the cap is
// exceeded, dropping the oldest by CreatedAt (the ID breaking ties). A
// non-positive maxSessions keeps the collection unbounded. The slice is
// sorted in place, so after an eviction the survivors are stored oldest
// first.
func (ss *SessionService) evict(sessions []*Session) []*Session {
	if ss.maxSessions <= 0 || len(sessions) <= ss.maxSessions {
		return sessions
	}
	excess := len(sessions) - ss.maxSessions
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})
	return sessions[excess:]
}

// sessionExpired reports whether s has expired at or before now. Validate
// and PurgeExpired share this definition so a token check and a purge
// agree on the boundary.
func sessionExpired(s *Session, now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// newToken returns a 64-character hex token (32 random bytes),
// cryptographically strong enough for bearer-token use.
func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to
		// two time-based ids so the process can keep serving.
		return newID() + newID()
	}
	return hex.EncodeToString(buf)
}

// AuthService ties the user and session services together: Login verifies
// credentials through UserService and opens a session through
// SessionService; Authenticate and Logout are thin proxies over the
// session collection. store is kept for future login auditing.
type AuthService struct {
	store    *Store
	users    *UserService
	sessions *SessionService
	ttl      time.Duration
}

// NewAuthService returns an AuthService that verifies credentials with
// users, manages sessions with sessions and opens sessions valid for ttl.
// ttl must be positive; a non-positive ttl makes every Login fail with
// ErrInvalid.
func NewAuthService(store *Store, users *UserService, sessions *SessionService, ttl time.Duration) *AuthService {
	return &AuthService{store: store, users: users, sessions: sessions, ttl: ttl}
}

// Login authenticates username with plainPassword and opens a session
// valid for the service's ttl. Every failure mode — an unknown user, a
// disabled user, a wrong password — returns ErrInvalidCredentials, so
// callers cannot tell which users exist (no username enumeration). The
// returned session carries the user's ID and a username snapshot.
//
// Password verification delegates to user.go's unexported verifyPassword
// (same package): a malformed stored hash simply fails verification, which
// surfaces as ErrInvalidCredentials like any other bad login.
func (as *AuthService) Login(username, plainPassword string) (*Session, error) {
	user, err := as.users.GetByUsername(username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if !user.Enabled {
		return nil, ErrInvalidCredentials
	}
	if !verifyPassword(plainPassword, user.PasswordHash) {
		return nil, ErrInvalidCredentials
	}
	return as.sessions.Create(user.ID, user.Username, as.ttl)
}

// Logout invalidates the session with the given token. An unknown token is
// reported with ErrNotFound.
func (as *AuthService) Logout(token string) error {
	return as.sessions.Delete(token)
}

// Authenticate returns the session for token if it is valid (exists and
// not expired). Expired sessions are dropped and reported as not found.
func (as *AuthService) Authenticate(token string) (*Session, error) {
	return as.sessions.Validate(token)
}

// Resource names used with CanAccess. Each names a group of operations the
// console exposes: media library, playlists (program schedules), scheduled
// tasks, outputs (and output failover control), scenes, user management,
// the audit log and webhook subscriptions.
const (
	ResourceMedia    = "media"
	ResourcePlaylist = "playlist"
	ResourceTask     = "task"
	ResourceOutput   = "output"
	ResourceScene    = "scene"
	ResourceUser     = "user"
	ResourceAudit    = "audit"
	ResourceWebhook  = "webhook"
)

// Action names used with CanAccess.
const (
	// ActionRead grants read access to a resource.
	ActionRead = "read"
	// ActionWrite grants create/update/delete (control) access to a
	// resource.
	ActionWrite = "write"
)

// CanAccess reports whether role may perform action on resource. The
// permission matrix:
//
//	admin:    any resource, any action (full control), unconditionally.
//	operator: read/write media, playlists, tasks, outputs, scenes and
//	          webhooks; read-only audit; no access to user management.
//	auditor:  read-only on every resource; never write (no user
//	          management, no output control).
//
// Unknown roles, resources and actions are denied (default deny), except
// that admin is unconditional and auditor read is granted for any
// resource: newly added resources stay read-only for auditors
// automatically, while operators need an explicit matrix update.
func CanAccess(role UserRole, resource, action string) bool {
	switch role {
	case RoleAdmin:
		return true
	case RoleOperator:
		return operatorCanAccess(resource, action)
	case RoleAuditor:
		return action == ActionRead
	}
	return false
}

// operatorCanAccess is the operator row of the permission matrix: an
// explicit allowlist of (resource, action) pairs. Anything absent —
// user management, audit writes, unknown resources or actions — is denied.
func operatorCanAccess(resource, action string) bool {
	switch action {
	case ActionRead:
		switch resource {
		case ResourceMedia, ResourcePlaylist, ResourceTask, ResourceOutput, ResourceScene, ResourceWebhook, ResourceAudit:
			return true
		}
	case ActionWrite:
		switch resource {
		case ResourceMedia, ResourcePlaylist, ResourceTask, ResourceOutput, ResourceScene, ResourceWebhook:
			return true
		}
	}
	return false
}
