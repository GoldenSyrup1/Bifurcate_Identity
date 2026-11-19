package migration

// Phase is the staged-rollout state for one tenant's migration off the
// legacy identity store. This is the literal mechanism behind "safe,
// incremental migration off legacy identity services using feature flags,
// backwards compatibility and staged rollout" — each phase is a flag value,
// and the only way to reach NewOnly is by passing through every stage before
// it, on purpose, one tenant at a time.
type Phase string

const (
	// LegacyOnly: nothing has changed yet. All reads and writes hit the
	// legacy store. This is the starting state for every tenant.
	LegacyOnly Phase = "legacy_only"

	// DualWrite: writes go to both stores so the modern store starts
	// filling up with real data, but reads still come from legacy. If the
	// modern write fails or the process dies mid-write, the client-visible
	// behavior is unchanged — this is the backwards-compatibility
	// guarantee, not a hope.
	DualWrite Phase = "dual_write"

	// ShadowVerify: still answering reads from legacy, but now also reading
	// from modern on every request and diffing the two. Drift gets logged
	// here, before it's load-bearing — this is how migrations catch bugs
	// before they catch users.
	ShadowVerify Phase = "shadow_verify"

	// NewOnly: cutover. Reads and writes go to modern exclusively. Legacy
	// is left as-is (not deleted) so there's a rollback path.
	NewOnly Phase = "new_only"
)

var order = map[Phase]int{
	LegacyOnly:   0,
	DualWrite:    1,
	ShadowVerify: 2,
	NewOnly:      3,
}

// Next returns the next phase in the staged rollout, or ok=false if already
// at the end. Phases cannot be skipped — advancing is always one step.
func (p Phase) Next() (Phase, bool) {
	switch p {
	case LegacyOnly:
		return DualWrite, true
	case DualWrite:
		return ShadowVerify, true
	case ShadowVerify:
		return NewOnly, true
	default:
		return p, false
	}
}

func (p Phase) valid() bool {
	_, ok := order[p]
	return ok
}
