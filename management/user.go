package management

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// UserRole is the permission level of a console user.
type UserRole string

const (
	// RoleAdmin is a full-access administrator.
	RoleAdmin UserRole = "admin"
	// RoleOperator controls playback and scheduling.
	RoleOperator UserRole = "operator"
	// RoleAuditor may only view state and the audit log.
	RoleAuditor UserRole = "auditor"
)

// User is a console login account. PasswordHash never holds plaintext: it
// stores the salt$hex digest produced by hashPassword, assigned only by
// UserService.Create and UserService.SetPassword.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Role         UserRole  `json:"role"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// UserSpec is the validated input used to create or replace a user.
// Password is the plaintext password, which the service hashes before it
// is stored: it never appears in the persisted document. PasswordHash is
// deliberately not part of the spec — only Create and SetPassword assign
// the stored digest.
type UserSpec struct {
	Username string
	Password string
	Role     UserRole
	Enabled  bool
}

// minPasswordLength is the minimum accepted plaintext password length in
// bytes.
const minPasswordLength = 8

// passwordHashIterations is the number of SHA-256 rounds applied to a
// salted password by hashPassword and verifyPassword.
const passwordHashIterations = 10000

// hashPassword derives a salted password digest using only the standard
// library: a random 16-byte salt and passwordHashIterations rounds of
// SHA-256 over the salted password, stored as hex(salt) + "$" + hex(hash).
// This is a deliberate trade-off: it is not bcrypt/argon2 strength and
// offers limited protection against offline brute force, but it is
// adequate for the local single-machine console where the store file is
// already the trust boundary. The random per-user salt makes identical
// passwords produce distinct digests and defeats rainbow tables.
func hashPassword(plain string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("user: generate password salt: %w", err)
	}
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(derivePasswordHash(salt, plain)), nil
}

// verifyPassword reports whether plain matches the stored salt$hex digest.
// The stored value is split back into salt and digest and recomputed with
// the same iteration count; the comparison uses subtle.ConstantTimeCompare
// so a mismatch takes as long as a match. Malformed stored values fail
// verification.
func verifyPassword(plain, stored string) bool {
	saltHex, hashHex, ok := splitPasswordHash(stored)
	if !ok {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := hex.DecodeString(hashHex)
	if err != nil {
		return false
	}
	got := derivePasswordHash(salt, plain)
	return subtle.ConstantTimeCompare(got, expected) == 1
}

// splitPasswordHash splits a stored salt$hex digest into its two halves.
func splitPasswordHash(stored string) (saltHex, hashHex string, ok bool) {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// derivePasswordHash folds the salted password through the configured
// number of SHA-256 rounds.
func derivePasswordHash(salt []byte, plain string) []byte {
	sum := sha256.Sum256(append(salt, []byte(plain)...))
	h := sum[:]
	for i := 1; i < passwordHashIterations; i++ {
		sum = sha256.Sum256(h)
		h = sum[:]
	}
	return h
}

// UserService provides CRUD over the users of a Store plus password
// management. Password hashing happens inside the service: specs carry the
// plaintext password and the store document only ever sees the digest.
type UserService struct {
	store *Store
}

// NewUserService returns a UserService backed by store.
func NewUserService(store *Store) *UserService {
	return &UserService{store: store}
}

// List returns all users sorted by username.
func (us *UserService) List() []*User {
	out := make([]*User, 0)
	us.store.View(func(d *Data) {
		out = append(out, d.Users...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Get returns the user with the given id.
func (us *UserService) Get(id string) (*User, error) {
	var found *User
	us.store.View(func(d *Data) {
		for _, u := range d.Users {
			if u.ID == id {
				found = u
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("user %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// GetByUsername returns the user with the given username, for login. The
// username is matched exactly against the stored (trimmed) value.
func (us *UserService) GetByUsername(username string) (*User, error) {
	var found *User
	us.store.View(func(d *Data) {
		for _, u := range d.Users {
			if u.Username == username {
				found = u
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("user %q: %w", username, ErrNotFound)
	}
	return found, nil
}

// Create adds a new user from spec. The username must be non-empty after
// trimming (ErrInvalid) and unique among users (ErrExists); the password
// must be at least minPasswordLength bytes (ErrInvalid) and the role one
// of the known roles (ErrInvalid). The password is hashed before storage
// and never persisted in plaintext.
func (us *UserService) Create(spec UserSpec) (*User, error) {
	spec, err := validateUserSpec(spec)
	if err != nil {
		return nil, err
	}
	hash, err := hashPassword(spec.Password)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &User{
		ID:           newID(),
		Username:     spec.Username,
		PasswordHash: hash,
		Role:         spec.Role,
		Enabled:      spec.Enabled,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err = us.store.Update(func(d *Data) error {
		for _, exist := range d.Users {
			if exist.Username == u.Username {
				return fmt.Errorf("user %q: %w", u.Username, ErrExists)
			}
		}
		d.Users = append(d.Users, u)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Update replaces the profile of the user with the given id from spec:
// username, role and the enabled flag are all replaced. The password is
// intentionally left untouched — use SetPassword. The new username must be
// non-empty (ErrInvalid) and must not collide with another user
// (ErrExists); renaming to its own current name is allowed. It returns the
// updated user.
func (us *UserService) Update(id string, spec UserSpec) (*User, error) {
	spec.Username = strings.TrimSpace(spec.Username)
	if err := validateUserProfile(spec); err != nil {
		return nil, err
	}
	var out *User
	err := us.store.Update(func(d *Data) error {
		var u *User
		for _, cand := range d.Users {
			if cand.ID == id {
				u = cand
				break
			}
		}
		if u == nil {
			return fmt.Errorf("user %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.Users {
			if exist.ID != id && exist.Username == spec.Username {
				return fmt.Errorf("user %q: %w", spec.Username, ErrExists)
			}
		}
		u.Username = spec.Username
		u.Role = spec.Role
		u.Enabled = spec.Enabled
		u.UpdatedAt = time.Now()
		out = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetPassword replaces the password of the user with the given id. The new
// password must be at least minPasswordLength bytes (ErrInvalid); it is
// hashed before storage and never persisted in plaintext.
func (us *UserService) SetPassword(id, plain string) error {
	if len(plain) < minPasswordLength {
		return fmt.Errorf("user: %w: password must be at least %d bytes", ErrInvalid, minPasswordLength)
	}
	hash, err := hashPassword(plain)
	if err != nil {
		return err
	}
	return us.update(id, func(u *User) error {
		u.PasswordHash = hash
		return nil
	})
}

// SetEnabled toggles the enabled flag of the user. Setting a flag to its
// current value is not special-cased: like the other SetEnabled
// implementations of the package it rewrites the store and bumps
// UpdatedAt.
func (us *UserService) SetEnabled(id string, enabled bool) error {
	return us.update(id, func(u *User) error {
		u.Enabled = enabled
		return nil
	})
}

// Delete removes the user with the given id. Sessions issued to the user
// are owned by the auth service and are not touched here.
func (us *UserService) Delete(id string) error {
	return us.store.Update(func(d *Data) error {
		idx := -1
		for i, u := range d.Users {
			if u.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("user %q: %w", id, ErrNotFound)
		}
		d.Users = append(d.Users[:idx], d.Users[idx+1:]...)
		return nil
	})
}

// update applies fn to the user with the given id under the store write
// lock; fn may mutate the user in place. Returning an error rolls back.
func (us *UserService) update(id string, fn func(u *User) error) error {
	return us.store.Update(func(d *Data) error {
		for _, u := range d.Users {
			if u.ID != id {
				continue
			}
			if err := fn(u); err != nil {
				return err
			}
			u.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("user %q: %w", id, ErrNotFound)
	})
}

// validateUserSpec performs field-level validation independent of the
// store: the username must be non-empty after trimming (returned trimmed),
// the password must meet the minimum length and the role must be one of
// the known roles.
func validateUserSpec(spec UserSpec) (UserSpec, error) {
	spec.Username = strings.TrimSpace(spec.Username)
	if err := validateUserProfile(spec); err != nil {
		return spec, err
	}
	if len(spec.Password) < minPasswordLength {
		return spec, fmt.Errorf("user: %w: password must be at least %d bytes", ErrInvalid, minPasswordLength)
	}
	return spec, nil
}

// validateUserProfile validates the username and role fields of a spec,
// without touching the password (which Update does not apply). The
// username must be non-empty after trimming.
func validateUserProfile(spec UserSpec) error {
	if strings.TrimSpace(spec.Username) == "" {
		return fmt.Errorf("user: %w: empty username", ErrInvalid)
	}
	return validateUserRole(spec.Role)
}

// validateUserRole reports whether the role is one of the known roles.
func validateUserRole(r UserRole) error {
	switch r {
	case RoleAdmin, RoleOperator, RoleAuditor:
		return nil
	}
	return fmt.Errorf("user: %w: unknown role %q", ErrInvalid, r)
}
