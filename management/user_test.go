package management

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// testPassword is used whenever a test creates or resets a password. It is
// long enough to pass the minimum-length check and contains non-hex
// characters, so asserting the store file does not contain it as a byte
// sequence is airtight (it can never appear inside the hex-encoded digest).
const testPassword = "S3cret!Pass#word"

// mustCreateUser creates a user from spec and fails the test on error.
func mustCreateUser(t *testing.T, us *UserService, spec UserSpec) *User {
	t.Helper()
	u, err := us.Create(spec)
	if err != nil {
		t.Fatalf("create user %q: %v", spec.Username, err)
	}
	return u
}

// validStoredHash reports whether h has the salt$hex shape produced by
// hashPassword: a 16-byte hex salt, "$", then the 32-byte hex digest.
func validStoredHash(h string) bool {
	parts := strings.SplitN(h, "$", 2)
	if len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 64 {
		return false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil
}

func TestUserCreateAndList(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	zeta := mustCreateUser(t, us, UserSpec{Username: "zeta", Password: testPassword, Role: RoleAuditor, Enabled: true})
	alpha := mustCreateUser(t, us, UserSpec{Username: "alpha", Password: testPassword, Role: RoleAdmin, Enabled: false})
	bravo := mustCreateUser(t, us, UserSpec{Username: "bravo", Password: testPassword, Role: RoleOperator, Enabled: true})

	// List is sorted by username.
	got := us.List()
	if len(got) != 3 {
		t.Fatalf("expected 3 users, got %d", len(got))
	}
	want := []string{"alpha", "bravo", "zeta"}
	for i, name := range want {
		if got[i].Username != name {
			t.Fatalf("expected user %d to be %q, got %q", i, name, got[i].Username)
		}
	}

	// Get returns the entity with all fields and timestamps set.
	u, err := us.Get(zeta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "zeta" || u.Role != RoleAuditor || !u.Enabled {
		t.Fatalf("unexpected user: %+v", u)
	}
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		t.Fatalf("expected timestamps set, got %+v", u)
	}
	if u.PasswordHash == "" || u.PasswordHash == testPassword {
		t.Fatal("expected a hashed password, not the plaintext")
	}
	if alpha.Role != RoleAdmin || alpha.Enabled {
		t.Fatalf("unexpected alpha: %+v", alpha)
	}
	if bravo.Role != RoleOperator || !bravo.Enabled {
		t.Fatalf("unexpected bravo: %+v", bravo)
	}
}

func TestUserCreateTrimsUsername(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "  alice  ", Password: testPassword, Role: RoleOperator, Enabled: true})
	if u.Username != "alice" {
		t.Fatalf("expected trimmed username, got %q", u.Username)
	}
	// The trimmed name is what login resolves.
	found, err := us.GetByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != u.ID {
		t.Fatal("expected GetByUsername to find the trimmed user")
	}
}

func TestUserCreateValidation(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	// empty username
	if _, err := us.Create(UserSpec{Username: "   ", Password: testPassword, Role: RoleAdmin, Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty username, got %v", err)
	}
	// password below the minimum length
	for _, short := range []string{"", "1234567"} {
		if _, err := us.Create(UserSpec{Username: "u", Password: short, Role: RoleAdmin, Enabled: true}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for short password %q, got %v", short, err)
		}
	}
	// unknown role
	if _, err := us.Create(UserSpec{Username: "u", Password: testPassword, Role: "root", Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown role, got %v", err)
	}

	// exactly the minimum length is accepted
	if _, err := us.Create(UserSpec{Username: "min", Password: "12345678", Role: RoleOperator, Enabled: true}); err != nil {
		t.Fatalf("expected an 8-byte password to be accepted, got %v", err)
	}
	if len(us.List()) != 1 {
		t.Fatalf("expected 1 user, got %d", len(us.List()))
	}
}

func TestUserUsernameUnique(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	alice := mustCreateUser(t, us, UserSpec{Username: "alice", Password: testPassword, Role: RoleAdmin, Enabled: true})
	mustCreateUser(t, us, UserSpec{Username: "bob", Password: testPassword, Role: RoleOperator, Enabled: true})

	// duplicate create
	if _, err := us.Create(UserSpec{Username: "alice", Password: testPassword, Role: RoleAdmin, Enabled: true}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate username, got %v", err)
	}
	// update onto another user's name
	if _, err := us.Update(alice.ID, UserSpec{Username: "bob", Role: RoleAdmin, Enabled: true}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is allowed
	upd, err := us.Update(alice.ID, UserSpec{Username: "alice", Role: RoleAdmin, Enabled: false})
	if err != nil {
		t.Fatalf("expected self-rename to be allowed, got %v", err)
	}
	if upd.Username != "alice" || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
}

func TestUserUpdate(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "carol", Password: testPassword, Role: RoleAuditor, Enabled: true})

	// Let time pass so the UpdatedAt bump is observable.
	time.Sleep(10 * time.Millisecond)

	upd, err := us.Update(u.ID, UserSpec{Username: "carol2", Role: RoleOperator, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Username != "carol2" || upd.Role != RoleOperator || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if !upd.UpdatedAt.After(u.UpdatedAt) {
		t.Fatal("expected UpdatedAt to move forward after Update")
	}
	if upd.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt preserved")
	}

	// Update must not touch the password: the original one still verifies.
	if !verifyPassword(testPassword, upd.PasswordHash) {
		t.Fatal("expected Update to leave the password hash untouched")
	}

	// missing id
	if _, err := us.Update("missing", UserSpec{Username: "x", Role: RoleAdmin, Enabled: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUserStoresHashedPassword(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "alice", Password: testPassword, Role: RoleAdmin, Enabled: true})

	// The stored digest has the salt$hex shape and never holds plaintext.
	if u.PasswordHash == testPassword {
		t.Fatal("password stored in plaintext")
	}
	if !validStoredHash(u.PasswordHash) {
		t.Fatalf("expected salt$hex password hash, got %q", u.PasswordHash)
	}

	// The store file must not contain the plaintext password anywhere.
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(testPassword)) {
		t.Fatal("store file contains the plaintext password")
	}

	// Two users sharing a password get distinct digests (random salt).
	u2 := mustCreateUser(t, us, UserSpec{Username: "bob", Password: testPassword, Role: RoleOperator, Enabled: true})
	if u2.PasswordHash == u.PasswordHash {
		t.Fatal("expected identical passwords to hash differently per-user")
	}
	if !verifyPassword(testPassword, u2.PasswordHash) {
		t.Fatal("expected the second user's password to verify")
	}
}

func TestUserSetPassword(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "dave", Password: testPassword, Role: RoleOperator, Enabled: true})

	newPass := "N3w!P@ssword-1"
	if err := us.SetPassword(u.ID, newPass); err != nil {
		t.Fatal(err)
	}
	// SetPassword commits a fresh document, so re-fetch the user to observe
	// the new hash (copy-on-write: earlier pointers are stale).
	got, err := us.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(newPass, got.PasswordHash) {
		t.Fatal("expected the new password to verify after SetPassword")
	}
	if verifyPassword(testPassword, got.PasswordHash) {
		t.Fatal("expected the old password to stop verifying after SetPassword")
	}
	if !validStoredHash(got.PasswordHash) {
		t.Fatalf("expected salt$hex hash after SetPassword, got %q", got.PasswordHash)
	}

	// short password rejected
	if err := us.SetPassword(u.ID, "short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for short password, got %v", err)
	}
	// the failed SetPassword must not have changed the hash
	got, err = us.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(newPass, got.PasswordHash) {
		t.Fatal("expected failed SetPassword to leave the hash untouched")
	}

	// missing id
	if err := us.SetPassword("missing", newPass); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUserPasswordVerify(t *testing.T) {
	// round trip
	hash, err := hashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(testPassword, hash) {
		t.Fatal("expected the correct password to verify")
	}
	// wrong password
	if verifyPassword("Wrong!Pass#word", hash) {
		t.Fatal("expected a wrong password to fail verification")
	}
	// malformed stored values never verify
	for _, malformed := range []string{"", "no-dollar-sign", "$", "00$", "$00", "xyz$abc", "0000$00"} {
		if verifyPassword(testPassword, malformed) {
			t.Fatalf("expected malformed stored hash %q to fail verification", malformed)
		}
	}
}

func TestUserVerifyPasswordTamperedDigest(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)
	u := mustCreateUser(t, us, UserSpec{Username: "tamper", Password: testPassword, Role: RoleAdmin, Enabled: true})

	parts := strings.SplitN(u.PasswordHash, "$", 2)
	digest := []byte(parts[1])
	if digest[0] == 'f' {
		digest[0] = '0'
	} else {
		digest[0]++
	}
	tampered := parts[0] + "$" + string(digest)

	if verifyPassword(testPassword, tampered) {
		t.Fatal("expected a tampered digest to fail verification")
	}
	if !verifyPassword(testPassword, u.PasswordHash) {
		t.Fatal("expected the original digest to still verify")
	}
}

func TestUserSetEnabled(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "erin", Password: testPassword, Role: RoleOperator, Enabled: false})
	if u.Enabled {
		t.Fatal("expected user to start disabled")
	}

	if err := us.SetEnabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := us.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("expected user enabled after SetEnabled(true)")
	}

	if err := us.SetEnabled(u.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = us.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected user disabled after SetEnabled(false)")
	}

	if err := us.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUserDelete(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	a := mustCreateUser(t, us, UserSpec{Username: "a", Password: testPassword, Role: RoleAdmin, Enabled: true})
	b := mustCreateUser(t, us, UserSpec{Username: "b", Password: testPassword, Role: RoleOperator, Enabled: true})

	if err := us.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(us.List()) != 1 || us.List()[0].ID != b.ID {
		t.Fatalf("expected only b to remain, got %+v", us.List())
	}
	if _, err := us.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for deleted user, got %v", err)
	}
	if err := us.Delete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
	if err := us.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing user, got %v", err)
	}
}

func TestUserGetByUsername(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "frank", Password: testPassword, Role: RoleAuditor, Enabled: true})
	found, err := us.GetByUsername("frank")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != u.ID {
		t.Fatal("expected GetByUsername to return the created user")
	}

	if _, err := us.GetByUsername("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestUserSetEnabledSameValueRewrites verifies the package convention that
// SetEnabled is never special-cased: setting a flag to its current value
// still rewrites the store file and bumps root UpdatedAt.
func TestUserSetEnabledSameValueRewrites(t *testing.T) {
	s := newTestStore(t)
	us := NewUserService(s)

	u := mustCreateUser(t, us, UserSpec{Username: "gina", Password: testPassword, Role: RoleOperator, Enabled: true})

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
	updatedBefore := u.UpdatedAt

	// Let time pass so a rewrite is observable both in the file bytes and
	// in the timestamps.
	time.Sleep(10 * time.Millisecond)

	if err := us.SetEnabled(u.ID, true); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("expected SetEnabled with an unchanged value to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.UpdatedAt.Equal(rootBefore) {
		t.Fatal("expected root UpdatedAt to bump on SetEnabled")
	}
	// re-fetch: the Update committed a fresh document, so the original
	// pointer's UpdatedAt was never touched.
	got, err := us.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Equal(updatedBefore) {
		t.Fatal("expected user UpdatedAt to bump on SetEnabled")
	}
}
