package migration

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bifurcate-identity/internal/store"
)

// Event is one line in a tenant's migration timeline — what the dashboard's
// live log is built from.
type Event struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// Tenant is one organisation being migrated. Migration is always per-tenant:
// that's the whole point of staged rollout — tenant 3 can sit at
// shadow_verify while tenant 1 is already cut over, and a bad surprise with
// one tenant never touches another.
type Tenant struct {
	ID    string
	Name  string
	Phase Phase
}

// Migrator owns both backends and every tenant's migration state. It is the
// single place that decides, for a given tenant and a given operation,
// which store(s) to touch — callers (the HTTP layer) never talk to a store
// directly.
type Migrator struct {
	mu      sync.Mutex
	legacy  store.Store
	modern  store.Store
	tenants map[string]*Tenant
	seen    map[string]map[string]bool // tenantID -> requestID -> processed (idempotency)
	log     map[string][]Event
	chaos   map[string]bool // tenantID -> armed: next dual-write will "crash" before the modern write
	nextID  int
}

func New() *Migrator {
	return &Migrator{
		legacy:  store.NewLegacyStore(),
		modern:  store.NewModernStore(),
		tenants: make(map[string]*Tenant),
		seen:    make(map[string]map[string]bool),
		log:     make(map[string][]Event),
		chaos:   make(map[string]bool),
	}
}

func (m *Migrator) record(tenantID, format string, args ...any) {
	m.log[tenantID] = append(m.log[tenantID], Event{At: time.Now(), Message: fmt.Sprintf(format, args...)})
	// cap the log so a demo left running doesn't grow unbounded
	if len(m.log[tenantID]) > 200 {
		m.log[tenantID] = m.log[tenantID][len(m.log[tenantID])-200:]
	}
}

// CreateTenant adds a new tenant starting at LegacyOnly — every tenant
// begins the same way; migration progress is earned, not assumed.
func (m *Migrator) CreateTenant(name string) *Tenant {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	t := &Tenant{ID: fmt.Sprintf("t%d", m.nextID), Name: name, Phase: LegacyOnly}
	m.tenants[t.ID] = t
	m.record(t.ID, "tenant %q created at phase legacy_only", name)
	return t
}

type TenantView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Phase       Phase  `json:"phase"`
	LegacyCount int    `json:"legacy_count"`
	ModernCount int    `json:"modern_count"`
	ChaosArmed  bool   `json:"chaos_armed"`
	CanAdvance  bool   `json:"can_advance"`
	NextPhase   Phase  `json:"next_phase,omitempty"`
}

func (m *Migrator) Tenants() []TenantView {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.tenants))
	for id := range m.tenants {
		ids = append(ids, id)
	}
	// Stable, deterministic order (by creation sequence) instead of Go's
	// randomized map iteration — otherwise tenant cards swap position
	// between renders and a click can land on the wrong tenant.
	sort.Slice(ids, func(i, j int) bool {
		return tenantSeq(ids[i]) < tenantSeq(ids[j])
	})

	out := make([]TenantView, 0, len(ids))
	for _, id := range ids {
		t := m.tenants[id]
		next, canAdvance := t.Phase.Next()
		v := TenantView{
			ID:          t.ID,
			Name:        t.Name,
			Phase:       t.Phase,
			LegacyCount: len(m.legacy.List(t.ID)),
			ModernCount: len(m.modern.List(t.ID)),
			ChaosArmed:  m.chaos[t.ID],
			CanAdvance:  canAdvance,
		}
		if canAdvance {
			v.NextPhase = next
		}
		out = append(out, v)
	}
	return out
}

// tenantSeq extracts the numeric sequence from an ID like "t12" → 12, so
// sorting is numeric rather than lexicographic (t2 < t10, not "t10" < "t2").
func tenantSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "t"))
	return n
}

// Advance moves a tenant to the next phase in the staged rollout. It never
// accepts an arbitrary target phase on purpose — a real migration control
// plane shouldn't let someone fat-finger a tenant straight from legacy_only
// to new_only.
func (m *Migrator) Advance(tenantID string) (Phase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return "", fmt.Errorf("unknown tenant %q", tenantID)
	}
	next, ok := t.Phase.Next()
	if !ok {
		return t.Phase, fmt.Errorf("tenant %q already at final phase %q", tenantID, t.Phase)
	}
	old := t.Phase
	t.Phase = next
	m.record(tenantID, "phase advanced: %s → %s", old, next)
	return next, nil
}

// ArmChaos marks the tenant's next dual-write signup to crash after the
// legacy write succeeds but before the modern write happens — the exact
// failure mode staged rollout exists to survive.
func (m *Migrator) ArmChaos(tenantID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[tenantID]; !ok {
		return fmt.Errorf("unknown tenant %q", tenantID)
	}
	m.chaos[tenantID] = true
	m.record(tenantID, "💥 chaos armed: next write will crash between legacy and modern")
	return nil
}

// Signup routes a new user through whichever backend(s) the tenant's phase
// requires, with idempotency on requestID so a client retry (the normal
// response to a timeout) never double-creates a user.
func (m *Migrator) Signup(tenantID, email, password, requestID string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tenants[tenantID]
	if !ok {
		return nil, fmt.Errorf("unknown tenant %q", tenantID)
	}

	if m.seen[tenantID] == nil {
		m.seen[tenantID] = make(map[string]bool)
	}
	if requestID != "" && m.seen[tenantID][requestID] {
		m.record(tenantID, "signup %s: duplicate request_id %s ignored (idempotent)", email, requestID)
		return m.tail(tenantID, 1), nil
	}
	if requestID != "" {
		m.seen[tenantID][requestID] = true
	}

	u := store.User{Email: email, PasswordHash: password, TenantID: tenantID, CreatedAt: time.Now()}
	before := len(m.log[tenantID])

	switch t.Phase {
	case LegacyOnly:
		m.legacy.Create(tenantID, u)
		m.record(tenantID, "signup %s: wrote to legacy only (phase=legacy_only)", email)

	case DualWrite, ShadowVerify:
		m.legacy.Create(tenantID, u)
		m.record(tenantID, "signup %s: wrote to legacy ✓", email)

		if m.chaos[tenantID] {
			m.chaos[tenantID] = false
			m.record(tenantID, "signup %s: 💥 simulated crash — process died before the modern write happened", email)
			m.record(tenantID, "signup %s: client sees a timeout; legacy already has the user, so no lockout — but modern is now missing this user until reconciliation runs", email)
			break
		}

		m.modern.Create(tenantID, u)
		m.record(tenantID, "signup %s: wrote to modern ✓ (dual-write complete)", email)

	case NewOnly:
		m.modern.Create(tenantID, u)
		m.record(tenantID, "signup %s: wrote to modern only (phase=new_only)", email)
	}

	return m.tailSince(tenantID, before), nil
}

// LoginResult reports which backend actually answered the read, and — in
// shadow_verify — whether that answer agreed with the other store.
type LoginResult struct {
	Found       bool   `json:"found"`
	AnsweredBy  string `json:"answered_by"`
	DriftFound  bool   `json:"drift_found"`
	DriftDetail string `json:"drift_detail,omitempty"`
}

func (m *Migrator) Login(tenantID, email, password string) (LoginResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[tenantID]
	if !ok {
		return LoginResult{}, fmt.Errorf("unknown tenant %q", tenantID)
	}

	switch t.Phase {
	case LegacyOnly, DualWrite:
		u, found := m.legacy.Get(tenantID, email)
		ok := found && u.PasswordHash == password
		m.record(tenantID, "login %s: answered by legacy (phase=%s), match=%v", email, t.Phase, ok)
		return LoginResult{Found: ok, AnsweredBy: "legacy"}, nil

	case ShadowVerify:
		lu, lok := m.legacy.Get(tenantID, email)
		answer := lok && lu.PasswordHash == password

		mu, mok := m.modern.Get(tenantID, email)
		modernAnswer := mok && mu.PasswordHash == password

		res := LoginResult{Found: answer, AnsweredBy: "legacy"}
		if answer != modernAnswer {
			res.DriftFound = true
			res.DriftDetail = fmt.Sprintf("legacy says found=%v, modern says found=%v — logged, not surfaced to the client", answer, modernAnswer)
			m.record(tenantID, "login %s: ⚠ drift detected during shadow-verify — %s", email, res.DriftDetail)
		} else {
			m.record(tenantID, "login %s: answered by legacy, modern agrees (shadow-verify clean)", email)
		}
		return res, nil

	default: // NewOnly
		u, found := m.modern.Get(tenantID, email)
		ok := found && u.PasswordHash == password
		m.record(tenantID, "login %s: answered by modern (phase=new_only), match=%v", email, ok)
		return LoginResult{Found: ok, AnsweredBy: "modern"}, nil
	}
}

// ReconcileReport is what a reconciliation pass finds and fixes.
type ReconcileReport struct {
	MissingInModern []string `json:"missing_in_modern"`
	MissingInLegacy []string `json:"missing_in_legacy"`
	Healed          int      `json:"healed"`
}

// Reconcile scans both stores for a tenant and backfills anything missing
// from modern (the direction that matters during migration: legacy is the
// source of truth until cutover). This is what turns "a node died mid-task"
// from a permanent inconsistency into a transient, self-correcting one.
func (m *Migrator) Reconcile(tenantID string) (ReconcileReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[tenantID]; !ok {
		return ReconcileReport{}, fmt.Errorf("unknown tenant %q", tenantID)
	}

	legacyUsers := m.legacy.List(tenantID)
	modernUsers := m.modern.List(tenantID)

	modernSet := make(map[string]bool, len(modernUsers))
	for _, u := range modernUsers {
		modernSet[u.Email] = true
	}
	legacySet := make(map[string]bool, len(legacyUsers))
	for _, u := range legacyUsers {
		legacySet[u.Email] = true
	}

	report := ReconcileReport{}
	for _, u := range legacyUsers {
		if !modernSet[u.Email] {
			report.MissingInModern = append(report.MissingInModern, u.Email)
			m.modern.Create(tenantID, u) // heal: replay the missing write
			report.Healed++
		}
	}
	for _, u := range modernUsers {
		if !legacySet[u.Email] {
			report.MissingInLegacy = append(report.MissingInLegacy, u.Email)
		}
	}

	if report.Healed > 0 {
		m.record(tenantID, "reconcile: found %d user(s) missing from modern, healed by backfill ✓", report.Healed)
	} else {
		m.record(tenantID, "reconcile: no drift found, stores agree")
	}
	return report, nil
}

func (m *Migrator) Log(tenantID string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.log[tenantID]))
	copy(out, m.log[tenantID])
	return out
}

// tail/tailSince must be called with m.mu already held.
func (m *Migrator) tail(tenantID string, n int) []Event {
	full := m.log[tenantID]
	if len(full) < n {
		n = len(full)
	}
	out := make([]Event, n)
	copy(out, full[len(full)-n:])
	return out
}

func (m *Migrator) tailSince(tenantID string, before int) []Event {
	full := m.log[tenantID]
	if before > len(full) {
		before = len(full)
	}
	out := make([]Event, len(full)-before)
	copy(out, full[before:])
	return out
}
