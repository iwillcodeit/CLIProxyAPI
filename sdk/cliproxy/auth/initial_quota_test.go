package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaTestAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", RegistrationEpoch: 1, CredentialVersion: 1, Metadata: map[string]any{"access_token": "test-token"}}
}
func quotaTestBody(now time.Time, used float64, reset time.Duration) []byte {
	window := func(used float64, reset time.Duration) map[string]any {
		return map[string]any{"utilization": used, "resets_at": now.Add(reset).Format(time.RFC3339)}
	}
	body, _ := json.Marshal(map[string]any{"five_hour": window(used, reset), "seven_day": window(10, 7*24*time.Hour), "seven_day_opus": nil, "seven_day_sonnet": nil})
	return body
}
func quotaTestPolicy(t *testing.T) (*InitialQuotaPolicy, time.Time, []*Auth) {
	t.Helper()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	p := newInitialQuotaPolicy()
	p.now = func() time.Time { return now }
	auths := []*Auth{quotaTestAuth("a"), quotaTestAuth("b")}
	for _, a := range auths {
		if err := p.Observe(a, quotaTestBody(now, 10, 4*time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	return p, now, auths
}

func TestInitialQuotaRankingAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*InitialQuotaPolicy, time.Time, []*Auth)
		model, want string
	}{
		{name: "soon reset", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			_ = p.Observe(a[1], quotaTestBody(now, 30, time.Hour), now)
		}, want: "b"},
		{name: "remaining quota", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			_ = p.Observe(a[0], quotaTestBody(now, 90, 4*time.Hour), now)
		}, want: "b"},
		{name: "reserve", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			_ = p.Observe(a[0], quotaTestBody(now, 99, time.Minute), now)
		}, want: "b"},
		{name: "shared weekly", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			v := p.observations[quotaIdentityOf(a[0])]
			v.sevenDay.used = .99
			p.observations[quotaIdentityOf(a[0])] = v
		}, want: "b"},
		{name: "opus weekly", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			v := p.observations[quotaIdentityOf(a[0])]
			v.opus = quotaWindow{known: true, used: .99, reset: now.Add(time.Hour)}
			p.observations[quotaIdentityOf(a[0])] = v
		}, want: "b"},
		{name: "sonnet ignores opus", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			v := p.observations[quotaIdentityOf(a[0])]
			v.opus = quotaWindow{known: true, used: 1, reset: now.Add(time.Hour)}
			p.observations[quotaIdentityOf(a[0])] = v
		}, model: "claude-sonnet-4-6", want: "a"},
		{name: "weights", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			a[1].Attributes = map[string]string{AttributeWeight: "5"}
		}, want: "b"},
		{name: "inactive window", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			_ = p.Observe(a[0], quotaTestBody(now, 90, 4*time.Hour), now)
			v := p.observations[quotaIdentityOf(a[1])]
			v.fiveHour = quotaWindow{known: true}
			p.observations[quotaIdentityOf(a[1])] = v
		}, want: "b"},
		{name: "new passive headers", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			a[0].Quota.ObserveResponseHeadersForProvider("claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.99"}, "Anthropic-Ratelimit-Unified-5h-Reset": {fmt.Sprint(now.Add(time.Minute).Unix())}}, now.Add(-time.Second))
			v := p.observations[quotaIdentityOf(a[0])]
			v.at = now.Add(-time.Minute)
			p.observations[quotaIdentityOf(a[0])] = v
		}, want: "b"},
		{name: "missing", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) { delete(p.observations, quotaIdentityOf(a[1])) }},
		{name: "stale", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			v := p.observations[quotaIdentityOf(a[1])]
			v.at = now.Add(-initialQuotaFreshness - time.Second)
			p.observations[quotaIdentityOf(a[1])] = v
		}},
		{name: "reset elapsed", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			v := p.observations[quotaIdentityOf(a[1])]
			v.fiveHour.reset = now
			p.observations[quotaIdentityOf(a[1])] = v
		}},
		{name: "replaced credential", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) { a[1].CredentialVersion++ }},
		{name: "API key", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) {
			a[1].Attributes = map[string]string{AttributeAPIKey: "key"}
		}},
		{name: "other provider", change: func(p *InitialQuotaPolicy, now time.Time, a []*Auth) { a[1].Provider = "codex" }},
		{name: "unknown model", model: "custom-alias"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, now, auths := quotaTestPolicy(t)
			if tc.change != nil {
				tc.change(p, now, auths)
			}
			model := tc.model
			if model == "" {
				model = "claude-opus-4-6"
			}
			got := p.pick(context.Background(), model, cliproxyexecutor.Options{Metadata: map[string]any{}}, auths)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want fallback, got %s", got.ID)
				}
			} else if got == nil || got.ID != tc.want {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}

func TestInitialQuotaRejectsMalformedSnapshotWithoutReplacingValidData(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	for _, body := range []string{`{}`, `null`, `{"five_hour":{"utilization":110},"seven_day":null}`, `{"five_hour":{"utilization":1,"resets_at":null},"seven_day":null}`, `{"five_hour":{"resets_at":"bad"},"seven_day":null}`} {
		if p.Observe(auths[0], []byte(body), now.Add(time.Minute)) == nil {
			t.Fatalf("accepted %s", body)
		}
		if !p.observations[quotaIdentityOf(auths[0])].at.Equal(now) {
			t.Fatal("invalid response replaced good observation")
		}
	}
}

func TestInitialQuotaConcurrentReservationsReleaseExpiryAndReload(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	var wg sync.WaitGroup
	counts := make(chan string, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := p.pick(context.Background(), "claude-opus-4-6", cliproxyexecutor.Options{Metadata: map[string]any{}}, auths)
			counts <- a.ID
		}()
	}
	wg.Wait()
	close(counts)
	n := map[string]int{}
	for id := range counts {
		n[id]++
	}
	if n["a"] != 50 || n["b"] != 50 {
		t.Fatalf("concurrent concentration: %v", n)
	}
	p.now = func() time.Time { return now.Add(initialQuotaReservationTTL) }
	opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
	_ = p.pick(context.Background(), "claude-opus-4-6", opts, auths)
	if len(p.reservations) != 1 {
		t.Fatal("reservations did not expire")
	}
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{QuotaAwareInitial: true})
	defer s.Stop()
	s.initialQuota = p
	replacement := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{QuotaAwareInitial: true})
	defer replacement.Stop()
	replacement.shareAffinityFrom(s)
	replacement.OnResult(Result{Options: opts}) // Release even when the normal result handler returns early.
	if replacement.initialQuota != p || len(p.reservations) != 0 {
		t.Fatal("reload lost observation/reservation ownership")
	}
}

func TestInitialQuotaPreservesBindingsParentsLCPAndRetry(t *testing.T) {
	for _, scenario := range []string{"explicit", "parent", "fork", "lcp", "lcp unavailable", "unavailable", "retry", "priority", "no session"} {
		t.Run(scenario, func(t *testing.T) {
			p, now, auths := quotaTestPolicy(t)
			s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, QuotaAwareInitial: true})
			defer s.Stop()
			s.initialQuota = p
			model := "claude-opus-4-6"
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"new"}}, Metadata: map[string]any{}}
			ctx := context.Background()
			switch scenario {
			case "explicit":
				s.cache.Set("claude::task:new::"+model, "a")
			case "parent":
				s.cache.Set("claude::task:parent::"+model, "a")
				opts.Headers.Set("X-Parent-Task-ID", "parent")
			case "fork":
				s.cache.Set("claude::session:parent::"+model, "a")
				opts.Headers = nil
				opts.OriginalRequest = []byte(`{"sessionId":"child","forkSource":{"sessionId":"parent"}}`)
			case "lcp", "lcp unavailable":
				opts.Headers = nil
				opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"shared prompt"}]}`)
				opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "caller"
				initial := auths[:1]
				if scenario == "lcp unavailable" {
					initial = []*Auth{quotaTestAuth("removed")}
				}
				if _, err := s.Pick(ctx, "claude", model, opts, initial); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				s.cache.Set("claude::task:new::"+model, "missing")
			case "retry":
				m := NewManager(nil, s, nil)
				ctx = m.withInitialQuotaSelection(ctx, s, model, map[string]struct{}{"previous": {}}, auths)
			case "priority":
				auths[0].Attributes = map[string]string{"priority": "5"}
			case "no session":
				opts.Headers = nil
			}
			// Quota now strongly prefers b, but all of these paths must retain a/fallback.
			if err := p.Observe(auths[1], quotaTestBody(now, 10, time.Hour), now); err != nil {
				t.Fatal(err)
			}
			a, err := s.Pick(ctx, "claude", model, opts, auths)
			if err != nil || a == nil || a.ID != "a" {
				t.Fatalf("%s: got %v, %v", scenario, a, err)
			}
		})
	}
	// A genuine cold session does use the quota ranking and binds the result.
	p, now, auths := quotaTestPolicy(t)
	_ = p.Observe(auths[1], quotaTestBody(now, 10, time.Hour), now)
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, QuotaAwareInitial: true})
	defer s.Stop()
	s.initialQuota = p
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"cold"}}, Metadata: map[string]any{}}
	a, err := s.Pick(context.Background(), "claude", "claude-opus-4-6", opts, auths)
	if err != nil || a.ID != "b" {
		t.Fatalf("cold selection: %v/%v", a, err)
	}
}

func TestInitialQuotaPollSchedule(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	a := auths[0]
	_ = p.Observe(a, quotaTestBody(now, 10, 2*time.Hour), now)
	if next := p.NextPoll(a, now, true); !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("hourly: %v", next)
	}
	before := now.Add(2*time.Hour - 30*time.Second)
	if next := p.NextPoll(a, now.Add(90*time.Minute), true); !next.Equal(before) {
		t.Fatalf("before reset: %v", next)
	}
	after := now.Add(2*time.Hour + 5*time.Second)
	if next := p.NextPoll(a, before, true); !next.Equal(after) {
		t.Fatalf("after reset: %v", next)
	}
	if next := p.NextPoll(a, after, true); !next.Equal(after.Add(time.Hour)) {
		t.Fatalf("repeated reset: %v", next)
	}
	if next := p.NextPoll(a, now, false); !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("failure: %v", next)
	}
}

func TestManagerInitialQuotaResolvesAliasAndSuppressesRetry(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint("mixed=", mixed), func(t *testing.T) {
			p, now, auths := quotaTestPolicy(t)
			s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, QuotaAwareInitial: true})
			s.initialQuota = p
			manager := NewManager(nil, s, nil)
			t.Cleanup(manager.StopAutoRefresh)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: "claude-opus-4-6", Alias: "quota-test-alias"}}})
			for _, a := range auths {
				if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-4-6"}, {ID: "quota-test-alias"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}
			loaded := manager.List()
			for _, a := range loaded {
				reset := 4 * time.Hour
				if a.ID == "b" {
					reset = time.Hour
				}
				if err := p.Observe(a, quotaTestBody(now, 10, reset), now); err != nil {
					t.Fatal(err)
				}
			}
			pick := func(session string, tried map[string]struct{}) *Auth {
				t.Helper()
				opts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {session}}, Metadata: map[string]any{}}
				var a *Auth
				var err error
				if mixed {
					a, _, _, err = manager.pickNextMixed(context.Background(), []string{"claude"}, "quota-test-alias", opts, tried)
				} else {
					a, _, err = manager.pickNext(context.Background(), "claude", "quota-test-alias", opts, tried)
				}
				if err != nil || a == nil {
					t.Fatalf("pick=%v/%v", a, err)
				}
				return a
			}
			if a := pick("initial", nil); a.ID != "b" {
				t.Fatalf("resolved alias selected %s, want b", a.ID)
			}
			if a := pick("retry", map[string]struct{}{"earlier-credential": {}}); a.ID != "a" {
				t.Fatalf("retry selected %s, want configured fallback a", a.ID)
			}
		})
	}
}

func TestInitialQuotaExecutionRetryRoundsUseFailover(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		t.Run(kind, func(t *testing.T) {
			p, now, auths := quotaTestPolicy(t)
			s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, QuotaAwareInitial: true})
			s.initialQuota = p
			manager := NewManager(nil, s, nil)
			t.Cleanup(manager.StopAutoRefresh)
			manager.SetRetryConfig(1, 0, 0)
			executor := &retryRoundCallExecutor{identifier: "claude"}
			manager.RegisterExecutor(executor)
			model := "claude-opus-4-6"
			for _, a := range auths {
				a.Metadata["disable_cooling"] = true
				if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}
			for _, a := range manager.List() {
				reset := 4 * time.Hour
				if a.ID == "b" {
					reset = time.Hour
				}
				if err := p.Observe(a, quotaTestBody(now, 10, reset), now); err != nil {
					t.Fatal(err)
				}
			}
			req := cliproxyexecutor.Request{Model: model}
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"retry-round"}}, Metadata: map[string]any{}}
			var err error
			switch kind {
			case "execute":
				_, err = manager.Execute(context.Background(), []string{"claude"}, req, opts)
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
			case "stream":
				_, err = manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
			}
			ids := executor.ids(kind)
			if err == nil || fmt.Sprint(ids) != "[b a a b]" {
				t.Fatalf("retry rounds=%v error=%v; want quota first, configured failover thereafter", ids, err)
			}
		})
	}
}

func TestInitialQuotaNewSubagentWithoutParentInheritanceIsCold(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	_ = p.Observe(auths[1], quotaTestBody(now, 10, time.Hour), now)
	inherit := false
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, QuotaAwareInitial: true, SubagentAffinity: &inherit})
	defer s.Stop()
	s.initialQuota = p
	model := "claude-opus-4-6"
	s.cache.Set("claude::task:parent::"+model, "a")
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"parent:agent:child"}, "X-Parent-Task-Id": {"parent"}}, Metadata: map[string]any{}}
	a, err := s.Pick(context.Background(), "claude", model, opts, auths)
	if err != nil || a == nil || a.ID != "b" {
		t.Fatalf("non-inheriting subagent=%v/%v, want quota-ranked b", a, err)
	}
}

func TestInitialQuotaChoiceChargesWeightedFallback(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	_ = p.Observe(auths[1], quotaTestBody(now, 10, time.Hour), now)
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &WeightedRoundRobinSelector{}, QuotaAwareInitial: true})
	defer s.Stop()
	s.initialQuota = p
	model := "claude-opus-4-6"
	var got []string
	for _, session := range []string{"cold-1", "cold-2", "", ""} {
		opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
		if session != "" {
			opts.Headers = http.Header{"X-Task-Id": {session}}
		}
		a, err := s.Pick(context.Background(), "claude", model, opts, auths)
		if err != nil || a == nil {
			t.Fatalf("pick %q: %v/%v", session, a, err)
		}
		got = append(got, a.ID)
	}
	// Quota-ranked sessions bind b; weighted routing then compensates with a.
	if fmt.Sprint(got) != "[b b a a]" {
		t.Fatalf("picks=%v, want [b b a a]", got)
	}
}
