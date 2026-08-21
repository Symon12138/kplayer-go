package management

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustCreateSession creates a session and fails the test on error.
func mustCreateSession(t *testing.T, ss *SessionService, userID, username string, ttl time.Duration) *Session {
	t.Helper()
	s, err := ss.Create(userID, username, ttl)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

// mustSeedAuthUser registers u in the store's Users collection directly,
// bypassing UserService, and computes PasswordHash from password via
// user.go's unexported hashPassword so the user can actually log in. The
// ID and timestamps are filled in when zero.
func mustSeedAuthUser(t *testing.T, s *Store, u *User, password string) *User {
	t.Helper()
	now := time.Now()
	u.ID = newID()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password for %q: %v", u.Username, err)
	}
	u.PasswordHash = hash
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = now
	}
	if err := s.Update(func(d *Data) error {
		d.Users = append(d.Users, u)
		return nil
	}); err != nil {
		t.Fatalf("seed user %q: %v", u.Username, err)
	}
	return u
}

func TestSessionCreateValidate(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)

	sess := mustCreateSession(t, ss, "u1", "alice", time.Hour)
	if len(sess.ID) != 64 {
		t.Fatalf("expected a 64-char token, got %d chars", len(sess.ID))
	}
	if !sess.CreatedAt.Before(sess.ExpiresAt) {
		t.Fatal("expected ExpiresAt after CreatedAt")
	}
	if !sess.ExpiresAt.After(time.Now()) {
		t.Fatal("expected a one-hour session to expire in the future")
	}

	got, err := ss.Validate(sess.ID)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.ID != sess.ID || got.UserID != "u1" || got.Username != "alice" {
		t.Fatalf("unexpected session: %+v", got)
	}

	// tokens are unique
	other := mustCreateSession(t, ss, "u2", "bob", time.Hour)
	if other.ID == sess.ID {
		t.Fatal("expected distinct tokens")
	}

	// an unknown token is not found
	if _, err := ss.Validate("deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown token, got %v", err)
	}
}

func TestSessionCreateRejectsNonPositiveTTL(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	if _, err := ss.Create("u1", "alice", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for zero ttl, got %v", err)
	}
	if _, err := ss.Create("u1", "alice", -time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative ttl, got %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("expected no sessions stored, got %d", len(snap.Sessions))
	}
}

func TestSessionValidateAndDeleteRejectEmptyToken(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	if _, err := ss.Validate(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty token, got %v", err)
	}
	if _, err := ss.Validate("   "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank token, got %v", err)
	}
	if err := ss.Delete(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty delete token, got %v", err)
	}
}

// TestSessionExpiredDropsOnValidate ages a session's ExpiresAt in the
// store (the pattern used by the alarm tests) so the expiry path is
// deterministic, then checks that Validate removes it and reports not
// found.
func TestSessionExpiredDropsOnValidate(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	sess := mustCreateSession(t, ss, "u1", "alice", time.Hour)

	past := time.Now().Add(-time.Minute)
	if err := s.Update(func(d *Data) error {
		for _, s2 := range d.Sessions {
			if s2.ID == sess.ID {
				s2.ExpiresAt = past
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ss.Validate(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for expired session, got %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("expected expired session removed, got %d sessions", len(snap.Sessions))
	}
}

// TestSessionExpiresInRealTime exercises the wall-clock path with a tiny
// ttl: the session must stop validating after the ttl elapses.
func TestSessionExpiresInRealTime(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	sess := mustCreateSession(t, ss, "u1", "alice", 20*time.Millisecond)

	if _, err := ss.Validate(sess.ID); err != nil {
		t.Fatalf("expected fresh session valid, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ss.Validate(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after ttl elapsed, got %v", err)
	}
}

func TestSessionDelete(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	sess := mustCreateSession(t, ss, "u1", "alice", time.Hour)

	if err := ss.Delete(sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ss.Validate(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	// deleting again reports not found
	if err := ss.Delete(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
}

func TestSessionPurgeExpired(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	live := mustCreateSession(t, ss, "u1", "alice", time.Hour)
	dead := mustCreateSession(t, ss, "u2", "bob", time.Hour)

	past := time.Now().Add(-time.Minute)
	if err := s.Update(func(d *Data) error {
		for _, s2 := range d.Sessions {
			if s2.ID == dead.ID {
				s2.ExpiresAt = past
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	n, err := ss.PurgeExpired(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged, got %d", n)
	}
	if _, err := ss.Validate(dead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected purged session gone, got %v", err)
	}
	if _, err := ss.Validate(live.ID); err != nil {
		t.Fatalf("expected live session kept, got %v", err)
	}
}

// TestSessionPurgeExpiredNoopNoWrite verifies that PurgeExpired with
// nothing to delete neither rewrites the store file nor bumps root
// UpdatedAt, and still returns zero removed.
func TestSessionPurgeExpiredNoopNoWrite(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s)
	mustCreateSession(t, ss, "u1", "alice", time.Hour)

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := snap.UpdatedAt

	time.Sleep(10 * time.Millisecond)

	n, err := ss.PurgeExpired(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 purged, got %d", n)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected PurgeExpired with nothing to delete not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
}

// TestSessionCapEvictsOldest verifies that creating past the configured
// cap evicts the oldest sessions (by CreatedAt) in the same update.
func TestSessionCapEvictsOldest(t *testing.T) {
	s := newTestStore(t)
	ss := NewSessionService(s, WithMaxSessions(3))

	first := mustCreateSession(t, ss, "u1", "alice", time.Hour)
	second := mustCreateSession(t, ss, "u1", "alice", time.Hour)
	third := mustCreateSession(t, ss, "u2", "bob", time.Hour)
	fourth := mustCreateSession(t, ss, "u3", "carol", time.Hour)

	// first is the oldest and must have been evicted
	if _, err := ss.Validate(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected oldest session evicted, got %v", err)
	}
	for _, keep := range []*Session{second, third, fourth} {
		if _, err := ss.Validate(keep.ID); err != nil {
			t.Fatalf("expected session %s kept, got %v", keep.ID, err)
		}
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 3 {
		t.Fatalf("expected 3 sessions after eviction, got %d", len(snap.Sessions))
	}
}

// TestSessionPersistsAcrossReopen verifies that a session survives a
// store reopen: the token stays valid against the freshly loaded document
// as long as it has not expired.
func TestSessionPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ss := NewSessionService(s)
	sess := mustCreateSession(t, ss, "u1", "alice", time.Hour)

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ss2 := NewSessionService(s2)
	got, err := ss2.Validate(sess.ID)
	if err != nil {
		t.Fatalf("validate after reopen: %v", err)
	}
	if got.ID != sess.ID || got.UserID != "u1" || got.Username != "alice" || !got.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("unexpected session after reopen: %+v", got)
	}
}

// TestAuthLoginSuccess seeds a user directly in the store and checks that
// Login opens a session that Authenticate accepts.
func TestAuthLoginSuccess(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)
	ss := NewSessionService(s)
	as := NewAuthService(s, us, ss, time.Hour)

	u := mustSeedAuthUser(t, s, &User{Username: "alice", Role: RoleOperator, Enabled: true}, "s3cret-pw")

	sess, err := as.Login("alice", "s3cret-pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.UserID != u.ID || sess.Username != "alice" {
		t.Fatalf("unexpected session: %+v", sess)
	}

	got, err := as.Authenticate(sess.ID)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != sess.ID || !got.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("unexpected authenticated session: %+v", got)
	}
}

// TestAuthLoginRejectsBadCredentials checks that every failure mode —
// unknown user, wrong password, disabled user — yields the same
// ErrInvalidCredentials, so the API cannot be used to enumerate users, and
// that no session is created for a failed login.
func TestAuthLoginRejectsBadCredentials(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)
	ss := NewSessionService(s)
	as := NewAuthService(s, us, ss, time.Hour)

	mustSeedAuthUser(t, s, &User{Username: "alice", Role: RoleOperator, Enabled: true}, "s3cret-pw")
	mustSeedAuthUser(t, s, &User{Username: "bob", Role: RoleAuditor, Enabled: false}, "pw")

	// unknown user
	if _, err := as.Login("nobody", "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
	// wrong password
	if _, err := as.Login("alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for wrong password, got %v", err)
	}
	// disabled user
	if _, err := as.Login("bob", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for disabled user, got %v", err)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("expected no sessions after failed logins, got %d", len(snap.Sessions))
	}
}

func TestAuthLogout(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)
	ss := NewSessionService(s)
	as := NewAuthService(s, us, ss, time.Hour)

	mustSeedAuthUser(t, s, &User{Username: "alice", Role: RoleOperator, Enabled: true}, "s3cret-pw")
	sess, err := as.Login("alice", "s3cret-pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := as.Logout(sess.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := as.Authenticate(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after logout, got %v", err)
	}
	// logging out again reports not found
	if err := as.Logout(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second logout, got %v", err)
	}
}

// TestCanAccess is the table-driven permission matrix: admin is
// unconditional, operator has an explicit read/write allowlist (audit
// read-only, no user management) and auditor is read-only everywhere.
func TestCanAccess(t *testing.T) {
	cases := []struct {
		name     string
		role     UserRole
		resource string
		action   string
		want     bool
	}{
		// admin: full control, including resources outside the matrix.
		{"admin read media", RoleAdmin, ResourceMedia, ActionRead, true},
		{"admin write media", RoleAdmin, ResourceMedia, ActionWrite, true},
		{"admin write user", RoleAdmin, ResourceUser, ActionWrite, true},
		{"admin read audit", RoleAdmin, ResourceAudit, ActionRead, true},
		{"admin write unknown", RoleAdmin, "billing", ActionWrite, true},

		// operator: read/write media, playlists, tasks, outputs, scenes
		// and webhooks; read-only audit; never user management.
		{"operator read media", RoleOperator, ResourceMedia, ActionRead, true},
		{"operator write media", RoleOperator, ResourceMedia, ActionWrite, true},
		{"operator read playlist", RoleOperator, ResourcePlaylist, ActionRead, true},
		{"operator write playlist", RoleOperator, ResourcePlaylist, ActionWrite, true},
		{"operator read task", RoleOperator, ResourceTask, ActionRead, true},
		{"operator write task", RoleOperator, ResourceTask, ActionWrite, true},
		{"operator read output", RoleOperator, ResourceOutput, ActionRead, true},
		{"operator write output", RoleOperator, ResourceOutput, ActionWrite, true},
		{"operator read scene", RoleOperator, ResourceScene, ActionRead, true},
		{"operator write scene", RoleOperator, ResourceScene, ActionWrite, true},
		{"operator read webhook", RoleOperator, ResourceWebhook, ActionRead, true},
		{"operator write webhook", RoleOperator, ResourceWebhook, ActionWrite, true},
		{"operator read audit", RoleOperator, ResourceAudit, ActionRead, true},
		{"operator write audit", RoleOperator, ResourceAudit, ActionWrite, false},
		{"operator read user", RoleOperator, ResourceUser, ActionRead, false},
		{"operator write user", RoleOperator, ResourceUser, ActionWrite, false},
		{"operator read unknown", RoleOperator, "billing", ActionRead, false},
		{"operator write unknown", RoleOperator, "billing", ActionWrite, false},

		// auditor: read-only on everything, never write.
		{"auditor read media", RoleAuditor, ResourceMedia, ActionRead, true},
		{"auditor write media", RoleAuditor, ResourceMedia, ActionWrite, false},
		{"auditor read user", RoleAuditor, ResourceUser, ActionRead, true},
		{"auditor write user", RoleAuditor, ResourceUser, ActionWrite, false},
		{"auditor read output", RoleAuditor, ResourceOutput, ActionRead, true},
		{"auditor write output", RoleAuditor, ResourceOutput, ActionWrite, false},
		{"auditor read audit", RoleAuditor, ResourceAudit, ActionRead, true},
		{"auditor write audit", RoleAuditor, ResourceAudit, ActionWrite, false},
		{"auditor read webhook", RoleAuditor, ResourceWebhook, ActionRead, true},
		{"auditor write webhook", RoleAuditor, ResourceWebhook, ActionWrite, false},

		// unknown roles and actions are denied; admin stays unconditional.
		{"unknown role", UserRole("root"), ResourceMedia, ActionRead, false},
		{"unknown action", RoleOperator, ResourceMedia, "execute", false},
		{"unknown action admin", RoleAdmin, ResourceMedia, "execute", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAccess(tc.role, tc.resource, tc.action); got != tc.want {
				t.Fatalf("CanAccess(%s, %q, %q) = %v, want %v", tc.role, tc.resource, tc.action, got, tc.want)
			}
		})
	}

	// exhaustive sweep over every known resource × action
	resources := []string{ResourceMedia, ResourcePlaylist, ResourceTask, ResourceOutput, ResourceScene, ResourceUser, ResourceAudit, ResourceWebhook}
	for _, r := range resources {
		for _, a := range []string{ActionRead, ActionWrite} {
			if !CanAccess(RoleAdmin, r, a) {
				t.Fatalf("admin denied %s %s", r, a)
			}
		}
		if !CanAccess(RoleAuditor, r, ActionRead) {
			t.Fatalf("auditor denied read %s", r)
		}
		if CanAccess(RoleAuditor, r, ActionWrite) {
			t.Fatalf("auditor granted write %s", r)
		}
	}
}
