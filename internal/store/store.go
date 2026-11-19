// Package store implements the two identity backends being migrated between:
// a "legacy" store (flat, single-tenant-unaware schema) and a "modern" store
// (org/membership-scoped, the shape the new CIAM platform actually wants).
//
// Both are in-memory for this demo — the point is the migration mechanism,
// not persistence. Swapping either for Postgres is a constructor change, not
// an architecture change: callers only see the Store interface.
package store

import (
	"sync"
	"time"
)

// User is what a signup produces. PasswordHash stands in for a real hash
// (bcrypt/argon2) — demo scope, so it's stored as given.
type User struct {
	Email        string
	PasswordHash string
	TenantID     string
	CreatedAt    time.Time
	// OrgID only has meaning in the modern schema — the legacy schema never
	// had a notion of org membership, which is exactly the kind of field
	// gap a real migration has to paper over.
	OrgID string
}

// Store is the shape both backends implement. A real migration usually
// starts with backends that DON'T implement the same interface — part of
// the engineering work is building this common seam.
type Store interface {
	Name() string
	Create(tenantID string, u User) error
	Get(tenantID, email string) (User, bool)
	List(tenantID string) []User
	Delete(tenantID, email string) error
}

// legacyStore: flat map, no concept of multi-tenant isolation beyond a
// manually-threaded tenantID key. This mirrors a lot of real legacy identity
// systems — tenancy was bolted on later, not designed in.
type legacyStore struct {
	mu    sync.RWMutex
	users map[string]map[string]User // tenantID -> email -> User
}

func NewLegacyStore() Store {
	return &legacyStore{users: make(map[string]map[string]User)}
}

func (s *legacyStore) Name() string { return "legacy" }

func (s *legacyStore) Create(tenantID string, u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[tenantID] == nil {
		s.users[tenantID] = make(map[string]User)
	}
	s.users[tenantID][u.Email] = u
	return nil
}

func (s *legacyStore) Get(tenantID, email string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[tenantID][email]
	return u, ok
}

func (s *legacyStore) List(tenantID string) []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users[tenantID]))
	for _, u := range s.users[tenantID] {
		out = append(out, u)
	}
	return out
}

func (s *legacyStore) Delete(tenantID, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users[tenantID], email)
	return nil
}

// modernStore: same shape here, but it's the one that gets the org/
// membership field populated and is where multi-tenant boundary checks
// would actually live in a real implementation.
type modernStore struct {
	mu    sync.RWMutex
	users map[string]map[string]User
}

func NewModernStore() Store {
	return &modernStore{users: make(map[string]map[string]User)}
}

func (s *modernStore) Name() string { return "modern" }

func (s *modernStore) Create(tenantID string, u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[tenantID] == nil {
		s.users[tenantID] = make(map[string]User)
	}
	s.users[tenantID][u.Email] = u
	return nil
}

func (s *modernStore) Get(tenantID, email string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[tenantID][email]
	return u, ok
}

func (s *modernStore) List(tenantID string) []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users[tenantID]))
	for _, u := range s.users[tenantID] {
		out = append(out, u)
	}
	return out
}

func (s *modernStore) Delete(tenantID, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users[tenantID], email)
	return nil
}
