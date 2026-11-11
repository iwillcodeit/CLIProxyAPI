package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	initialQuotaFreshness       = 75 * time.Minute
	initialQuotaReservationTTL  = time.Minute
	initialQuotaReservationKey  = "initial_quota_reservation"
	maxInitialQuotaReservations = 8192
)

type quotaIdentity struct {
	id             string
	epoch, version uint64
}

func quotaIdentityOf(a *Auth) quotaIdentity {
	return quotaIdentity{a.ID, a.RegistrationEpoch, a.CredentialVersion}
}

// ClaudeQuotaEligible excludes API keys and custom endpoints from subscription reads.
func ClaudeQuotaEligible(a *Auth) bool {
	return a != nil && !a.Disabled && a.Status != StatusDisabled && a.Provider == "claude" &&
		a.AuthKind() == AuthKindOAuth && authMetadataString(a, "access_token") != "" &&
		strings.TrimSpace(a.Attributes["base_url"]) == "" && authMetadataString(a, "base_url") == ""
}

type quotaWindow struct {
	inactive bool
	known    bool
	used     float64
	reset    time.Time
}
type initialQuotaObservation struct {
	at                               time.Time
	fiveHour, sevenDay, opus, sonnet quotaWindow
}
type quotaReservationState struct {
	identity quotaIdentity
	expires  time.Time
}
type initialQuotaReservation struct {
	policy *InitialQuotaPolicy
	ticket uint64
}

// InitialQuotaPolicy holds advisory usage snapshots. It never performs network I/O
// during selection and never changes existing bindings or credential metadata.
type InitialQuotaPolicy struct {
	mu           sync.Mutex
	observations map[quotaIdentity]initialQuotaObservation
	reservations map[uint64]quotaReservationState
	sequence     uint64
	now          func() time.Time
}

func newInitialQuotaPolicy() *InitialQuotaPolicy {
	return &InitialQuotaPolicy{observations: make(map[quotaIdentity]initialQuotaObservation), reservations: make(map[uint64]quotaReservationState), now: time.Now}
}

// InitialQuotaPolicy returns the current optional policy, shared across routing reloads.
func (m *Manager) InitialQuotaPolicy() *InitialQuotaPolicy {
	if s, ok := m.Selector().(*SessionAffinitySelector); ok {
		return s.initialQuota
	}
	return nil
}

// Observe records a validated OAuth usage response for this exact credential version.
// Missing windows remain unknown; explicit null means no currently active limit.
func (p *InitialQuotaPolicy) Observe(a *Auth, body []byte, at time.Time) error {
	if !ClaudeQuotaEligible(a) || len(body) > 1<<20 {
		return errors.New("invalid Claude usage observation")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return errors.New("invalid Claude usage JSON")
	}
	observation := initialQuotaObservation{at: at}
	for name, target := range map[string]*quotaWindow{"five_hour": &observation.fiveHour, "seven_day": &observation.sevenDay, "seven_day_opus": &observation.opus, "seven_day_sonnet": &observation.sonnet} {
		raw, exists := fields[name]
		if !exists {
			continue
		}
		target.known = true
		if strings.TrimSpace(string(raw)) == "null" {
			target.inactive = true
			continue
		}
		var value struct {
			Utilization *float64   `json:"utilization"`
			ResetsAt    *time.Time `json:"resets_at"`
		}
		if json.Unmarshal(raw, &value) != nil || value.Utilization == nil || math.IsNaN(*value.Utilization) || math.IsInf(*value.Utilization, 0) || *value.Utilization < 0 || *value.Utilization > 100 {
			return errors.New("invalid Claude usage window")
		}
		target.used = *value.Utilization / 100
		if value.ResetsAt != nil {
			target.reset = *value.ResetsAt
		}
		if target.reset.IsZero() && target.used != 0 {
			return errors.New("Claude usage window has no reset")
		}
	}
	if !observation.fiveHour.known || !observation.sevenDay.known {
		return errors.New("incomplete Claude usage response")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observations[quotaIdentityOf(a)] = observation
	return nil
}

// Retain drops observations for removed/replaced credentials without storing secrets.
func (p *InitialQuotaPolicy) Retain(auths []*Auth) {
	live := make(map[quotaIdentity]bool, len(auths))
	for _, a := range auths {
		if ClaudeQuotaEligible(a) {
			live[quotaIdentityOf(a)] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.observations {
		if !live[key] {
			delete(p.observations, key)
		}
	}
	for ticket, reservation := range p.reservations {
		if !live[reservation.identity] {
			delete(p.reservations, ticket)
		}
	}
}

// NextPoll schedules an hourly read plus one read before and after each known reset.
// Failed reads retry after a minute; the service permits only one in-flight read/account.
func (p *InitialQuotaPolicy) NextPoll(a *Auth, lastAttempt time.Time, succeeded bool) time.Time {
	if !succeeded {
		return lastAttempt.Add(time.Minute)
	}
	next := lastAttempt.Add(time.Hour)
	p.mu.Lock()
	observation := p.observations[quotaIdentityOf(a)]
	p.mu.Unlock()
	for _, window := range []quotaWindow{observation.fiveHour, observation.sevenDay, observation.opus, observation.sonnet} {
		if window.reset.IsZero() {
			continue
		}
		for _, at := range []time.Time{window.reset.Add(-30 * time.Second), window.reset.Add(5 * time.Second)} {
			if at.After(lastAttempt) && at.Before(next) {
				next = at
			}
		}
	}
	return next
}

type initialQuotaRetryRoundKey struct{}

func withInitialQuotaRetryRound(ctx context.Context, round int) context.Context {
	if round <= 0 {
		return ctx
	}
	return context.WithValue(ctx, initialQuotaRetryRoundKey{}, true)
}

type initialQuotaSelectionKey struct{}
type initialQuotaSelection struct {
	retry  bool
	models map[string]string
}

func (m *Manager) withInitialQuotaSelection(ctx context.Context, selector Selector, model string, tried map[string]struct{}, auths []*Auth) context.Context {
	s, ok := selector.(*SessionAffinitySelector)
	if !ok || s.initialQuota == nil {
		return ctx
	}
	retryRound, _ := ctx.Value(initialQuotaRetryRoundKey{}).(bool)
	selection := initialQuotaSelection{retry: retryRound || len(tried) > 0, models: make(map[string]string, len(auths))}
	for _, a := range auths {
		selection.models[a.ID] = m.selectionModelKeyForAuth(a, model)
	}
	return context.WithValue(ctx, initialQuotaSelectionKey{}, selection)
}

// pickInitial treats a quota-ranked choice like a binding for weighted accounting,
// so the configured strategy still compensates for it.
func (s *SessionAffinitySelector) pickInitial(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth, cold bool, reservation **weightedReservation) (*Auth, error) {
	var ranked *Auth
	if s.initialQuota != nil && cold {
		ranked = s.initialQuota.pick(ctx, model, opts, auths)
	}
	return s.pickWithAffinityReservation(ctx, provider, model, opts, auths, ranked, reservation)
}

func (p *InitialQuotaPolicy) pick(ctx context.Context, model string, opts cliproxyexecutor.Options, auths []*Auth) *Auth {
	var selection initialQuotaSelection
	if ctx != nil {
		selection, _ = ctx.Value(initialQuotaSelectionKey{}).(initialQuotaSelection)
	}
	if selection.retry || len(auths) < 2 || opts.Metadata == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	pending := make(map[quotaIdentity]int)
	for ticket, reservation := range p.reservations {
		if !reservation.expires.After(now) {
			delete(p.reservations, ticket)
		} else {
			pending[reservation.identity]++
		}
	}
	if len(p.reservations) >= maxInitialQuotaReservations {
		return nil
	}
	var best *Auth
	bestScore := -1.0
	for _, a := range auths {
		if !ClaudeQuotaEligible(a) || authWeight(a) <= 0 {
			return nil
		}
		identity := quotaIdentityOf(a)
		observation, ok := p.observations[identity]
		if !ok || observation.at.After(now) || now.Sub(observation.at) > initialQuotaFreshness {
			return nil
		}
		// Passive headers only augment a valid OAuth snapshot for matching credentials.
		if a.Quota.ObservedAt.After(observation.at) && !a.Quota.ObservedAt.After(now) && now.Sub(a.Quota.ObservedAt) <= initialQuotaFreshness {
			augmentQuotaWindow(&observation.fiveHour, a.Quota.Signals, "5h")
			augmentQuotaWindow(&observation.sevenDay, a.Quota.Signals, "7d")
		}
		resolved := model
		if v, exists := selection.models[a.ID]; exists {
			resolved = v
		}
		resolved = strings.ToLower(canonicalModelKey(resolved))
		windows := []quotaWindow{observation.fiveHour, observation.sevenDay}
		switch {
		case strings.Contains(resolved, "opus"):
			windows = append(windows, observation.opus)
		case strings.Contains(resolved, "sonnet"):
			windows = append(windows, observation.sonnet)
		case strings.Contains(resolved, "haiku"):
		default:
			return nil // Unknown aliases must not bypass a model-specific cap.
		}
		remaining := 1.0
		for _, window := range windows {
			if !window.known || (!window.reset.IsZero() && !window.reset.After(now)) {
				return nil
			}
			remaining = math.Min(remaining, 1-window.used)
		}
		horizon := 5 * time.Hour
		if !observation.fiveHour.reset.IsZero() {
			horizon = observation.fiveHour.reset.Sub(now)
		}
		horizon = max(5*time.Minute, min(5*time.Hour, horizon))
		// Keep a 2% advisory reserve; configured weights express relative plan capacity.
		score := math.Max(0, remaining-0.02) * float64(authWeight(a)) / horizon.Hours() / float64(1+pending[identity])
		if score > bestScore || (score == bestScore && (best == nil || a.ID < best.ID)) {
			best, bestScore = a, score
		}
	}
	if best == nil || bestScore <= 0 {
		return nil
	}
	p.sequence++
	p.reservations[p.sequence] = quotaReservationState{quotaIdentityOf(best), now.Add(initialQuotaReservationTTL)}
	opts.Metadata[initialQuotaReservationKey] = initialQuotaReservation{p, p.sequence}
	return best
}

func augmentQuotaWindow(window *quotaWindow, signals map[string]string, name string) {
	prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
	used, errUsed := strconv.ParseFloat(signals[http.CanonicalHeaderKey(prefix+"Utilization")], 64)
	reset, errReset := strconv.ParseInt(signals[http.CanonicalHeaderKey(prefix+"Reset")], 10, 64)
	if errUsed == nil && errReset == nil && !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0 && used <= 1 && reset > 0 {
		*window = quotaWindow{known: true, used: used, reset: time.Unix(reset, 0)}
	}
}

func releaseInitialQuotaReservation(metadata map[string]any) {
	reservation, ok := metadata[initialQuotaReservationKey].(initialQuotaReservation)
	if !ok || reservation.policy == nil {
		return
	}
	reservation.policy.mu.Lock()
	delete(reservation.policy.reservations, reservation.ticket)
	reservation.policy.mu.Unlock()
	delete(metadata, initialQuotaReservationKey)
}

// WindowPrimingDecision requires an explicit inactive window and a fresh OAuth
// observation. Zero utilization alone never authorizes generation. A positive
// reset is returned for confirming an already active five-hour window.
func (p *InitialQuotaPolicy) WindowPrimingDecision(a *Auth, now time.Time) (bool, time.Time) {
	if !ClaudeQuotaEligible(a) || authWeight(a) <= 0 {
		return false, time.Time{}
	}
	p.mu.Lock()
	observation, ok := p.observations[quotaIdentityOf(a)]
	p.mu.Unlock()
	if !ok || observation.at.After(now) || now.Sub(observation.at) > time.Minute || !observation.fiveHour.known {
		return false, time.Time{}
	}
	if a.Quota.ObservedAt.After(observation.at) && !a.Quota.ObservedAt.After(now) && now.Sub(a.Quota.ObservedAt) <= time.Minute {
		augmentQuotaWindow(&observation.fiveHour, a.Quota.Signals, "5h")
		augmentQuotaWindow(&observation.sevenDay, a.Quota.Signals, "7d")
	}
	if observation.fiveHour.reset.After(now) {
		return false, observation.fiveHour.reset
	}
	weekly := observation.sevenDay
	safeWeekly := weekly.known && weekly.used < .98 && (weekly.reset.IsZero() || weekly.reset.After(now))
	return observation.fiveHour.inactive && safeWeekly, time.Time{}
}
