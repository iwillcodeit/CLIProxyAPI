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

// quotaWindow is one observed limit. For the session window, known means the
// limit exists; a zero reset then means it is inactive. A zero weekly reset is
// treated as a full week away.
type quotaWindow struct {
	known bool
	used  float64
	reset time.Time
}

// scopedQuotaWindow is a model-scoped weekly cap; names are normalized match keys.
type scopedQuotaWindow struct {
	names []string
	quotaWindow
}

type initialQuotaObservation struct {
	at                time.Time
	session, sevenDay quotaWindow
	scoped            []scopedQuotaWindow
}

const (
	maxQuotaLimitEntries = 64
	minQuotaHorizon      = 15 * time.Minute
	maxQuotaHorizon      = 7 * 24 * time.Hour
)

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
// A null or absent session window with no session entry in limits means the
// account has no session limit. The shared weekly window is required.
func (p *InitialQuotaPolicy) Observe(a *Auth, body []byte, at time.Time) error {
	if !ClaudeQuotaEligible(a) || len(body) > 1<<20 {
		return errors.New("invalid Claude usage observation")
	}
	observation, err := parseClaudeUsage(body, at)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observations[quotaIdentityOf(a)] = observation
	return nil
}

type claudeUsageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type claudeUsageLimit struct {
	Kind     string     `json:"kind"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resets_at"`
	IsActive *bool      `json:"is_active"`
	Scope    *struct {
		Model *struct {
			ID          *string `json:"id"`
			DisplayName *string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func parseClaudeUsage(body []byte, at time.Time) (initialQuotaObservation, error) {
	var fields struct {
		FiveHour       json.RawMessage    `json:"five_hour"`
		SevenDay       *claudeUsageWindow `json:"seven_day"`
		SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
		SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
		Limits         []json.RawMessage  `json:"limits"`
	}
	observation := initialQuotaObservation{at: at}
	if json.Unmarshal(body, &fields) != nil || len(fields.Limits) > maxQuotaLimitEntries {
		return observation, errors.New("invalid Claude usage JSON")
	}
	if len(fields.FiveHour) > 0 && strings.TrimSpace(string(fields.FiveHour)) != "null" {
		// Only an explicit null reset marks the session inactive; an omitted one is unknown.
		var value claudeUsageWindow
		var keys map[string]json.RawMessage
		if json.Unmarshal(fields.FiveHour, &value) != nil || json.Unmarshal(fields.FiveHour, &keys) != nil {
			return observation, errors.New("invalid Claude session window")
		}
		window, err := quotaWindowFrom(value.Utilization, value.ResetsAt)
		if _, hasReset := keys["resets_at"]; err != nil || !hasReset || (window.reset.IsZero() && window.used != 0) {
			return observation, errors.New("invalid Claude session window")
		}
		mergeQuotaWindow(&observation.session, window)
	}
	if fields.SevenDay != nil {
		window, err := quotaWindowFrom(fields.SevenDay.Utilization, fields.SevenDay.ResetsAt)
		if err != nil {
			return observation, err
		}
		mergeQuotaWindow(&observation.sevenDay, window)
	}
	for name, legacy := range map[string]*claudeUsageWindow{"opus": fields.SevenDayOpus, "sonnet": fields.SevenDaySonnet} {
		if legacy == nil {
			continue
		}
		window, err := quotaWindowFrom(legacy.Utilization, legacy.ResetsAt)
		if err != nil {
			return observation, err
		}
		observation.scoped = append(observation.scoped, scopedQuotaWindow{[]string{name}, window})
	}
	for _, raw := range fields.Limits {
		var limit claudeUsageLimit
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &kind) != nil {
			return observation, errors.New("invalid Claude usage limit")
		}
		switch kind.Kind {
		case "session", "weekly_all", "weekly_scoped":
			if json.Unmarshal(raw, &limit) != nil {
				return observation, errors.New("invalid Claude usage limit")
			}
		default:
			continue // Unknown limit kinds are ignored.
		}
		switch limit.Kind {
		case "session":
			if limit.IsActive != nil && !*limit.IsActive {
				// An inactive session still exists; its reset is not yet scheduled.
				mergeQuotaWindow(&observation.session, quotaWindow{known: true})
				continue
			}
			window, err := quotaWindowFrom(limit.Percent, limit.ResetsAt)
			if err != nil || window.reset.IsZero() {
				return observation, errors.New("ambiguous Claude session limit")
			}
			mergeQuotaWindow(&observation.session, window)
		case "weekly_all":
			window, err := quotaWindowFrom(limit.Percent, limit.ResetsAt)
			if err != nil {
				return observation, err
			}
			mergeQuotaWindow(&observation.sevenDay, window)
		case "weekly_scoped":
			window, err := quotaWindowFrom(limit.Percent, limit.ResetsAt)
			if err != nil {
				return observation, err
			}
			var names []string
			if limit.Scope != nil && limit.Scope.Model != nil {
				for _, name := range []*string{limit.Scope.Model.DisplayName, limit.Scope.Model.ID} {
					if name != nil && quotaModelMatchKey(*name) != "" {
						names = append(names, quotaModelMatchKey(*name))
					}
				}
			}
			if len(names) > 0 {
				observation.scoped = append(observation.scoped, scopedQuotaWindow{names, window})
			}
		}
	}
	if !observation.sevenDay.known {
		return observation, errors.New("incomplete Claude usage response")
	}
	return observation, nil
}

func quotaWindowFrom(utilization *float64, resetsAt *time.Time) (quotaWindow, error) {
	if utilization == nil || math.IsNaN(*utilization) || math.IsInf(*utilization, 0) || *utilization < 0 || *utilization > 100 {
		return quotaWindow{}, errors.New("invalid Claude usage window")
	}
	window := quotaWindow{known: true, used: *utilization / 100}
	if resetsAt != nil {
		window.reset = *resetsAt
	}
	return window, nil
}

// mergeQuotaWindow combines duplicate representations conservatively: the higher
// utilization and the scheduled reset win, so an active window is never hidden.
func mergeQuotaWindow(dst *quotaWindow, src quotaWindow) {
	if !dst.known {
		*dst = src
		return
	}
	dst.used = math.Max(dst.used, src.used)
	if dst.reset.IsZero() {
		dst.reset = src.reset
	}
}

// quotaModelMatchKey lowercases and replaces separators so "Opus 5" matches "claude-opus-5".
func quotaModelMatchKey(name string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(strings.TrimSpace(name))), "-")
}

func claudeFamilyModel(resolved string) bool {
	return strings.HasPrefix(resolved, "claude") || strings.Contains(resolved, "opus") ||
		strings.Contains(resolved, "sonnet") || strings.Contains(resolved, "haiku")
}

// weeklyWindows returns the shared weekly window plus every scoped cap matching
// the resolved model. Non-Claude models are not comparable.
func (o initialQuotaObservation) weeklyWindows(resolved string) ([]quotaWindow, bool) {
	key := quotaModelMatchKey(resolved)
	if !claudeFamilyModel(key) {
		return nil, false
	}
	windows := []quotaWindow{o.sevenDay}
	for _, scoped := range o.scoped {
		for _, name := range scoped.names {
			if strings.Contains(key, name) {
				windows = append(windows, scoped.quotaWindow)
				break
			}
		}
	}
	return windows, true
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

// NextPoll schedules an hourly read plus one read before and after each known
// session or weekly reset, so a weekly reset is observed within seconds.
// Failed reads retry after a minute; the service permits only one in-flight read/account.
func (p *InitialQuotaPolicy) NextPoll(a *Auth, lastAttempt time.Time, succeeded bool) time.Time {
	if !succeeded {
		return lastAttempt.Add(time.Minute)
	}
	next := lastAttempt.Add(time.Hour)
	p.mu.Lock()
	observation := p.observations[quotaIdentityOf(a)]
	p.mu.Unlock()
	windows := []quotaWindow{observation.session, observation.sevenDay}
	for _, scoped := range observation.scoped {
		windows = append(windows, scoped.quotaWindow)
	}
	for _, window := range windows {
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
			augmentQuotaWindow(&observation.session, a.Quota.Signals, "5h")
			augmentQuotaWindow(&observation.sevenDay, a.Quota.Signals, "7d")
		}
		resolved := model
		if v, exists := selection.models[a.ID]; exists {
			resolved = v
		}
		weekly, comparable := observation.weeklyWindows(canonicalModelKey(resolved))
		if !comparable {
			return nil // Non-Claude aliases keep the configured strategy.
		}
		// The weekly allowance is use-it-or-lose-it: the soonest applicable weekly
		// reset sets the horizon. An active session window only limits remaining.
		remaining, horizon := 1.0, maxQuotaHorizon
		for _, window := range weekly {
			if !window.reset.IsZero() {
				horizon = min(horizon, window.reset.Sub(now))
			}
		}
		if observation.session.known && !observation.session.reset.IsZero() {
			weekly = append(weekly, observation.session)
		}
		for _, window := range weekly {
			if !window.known || (!window.reset.IsZero() && !window.reset.After(now)) {
				return nil
			}
			remaining = math.Min(remaining, 1-window.used)
		}
		horizon = max(minQuotaHorizon, horizon)
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

// WindowPrimingDecision requires an existing but inactive session limit and a
// fresh OAuth observation. Accounts without a session limit are never primed,
// and zero utilization alone never authorizes generation. A future session reset
// is returned for confirming an already active window.
func (p *InitialQuotaPolicy) WindowPrimingDecision(a *Auth, now time.Time) (bool, time.Time) {
	if !ClaudeQuotaEligible(a) || authWeight(a) <= 0 {
		return false, time.Time{}
	}
	p.mu.Lock()
	observation, ok := p.observations[quotaIdentityOf(a)]
	p.mu.Unlock()
	if !ok || observation.at.After(now) || now.Sub(observation.at) > time.Minute {
		return false, time.Time{}
	}
	if a.Quota.ObservedAt.After(observation.at) && !a.Quota.ObservedAt.After(now) && now.Sub(a.Quota.ObservedAt) <= time.Minute {
		augmentQuotaWindow(&observation.session, a.Quota.Signals, "5h")
		augmentQuotaWindow(&observation.sevenDay, a.Quota.Signals, "7d")
	}
	if observation.session.reset.After(now) {
		return false, observation.session.reset
	}
	weekly := observation.sevenDay
	safeWeekly := weekly.known && weekly.used < .98 && (weekly.reset.IsZero() || weekly.reset.After(now))
	inactive := observation.session.known && observation.session.reset.IsZero()
	return inactive && safeWeekly, time.Time{}
}
