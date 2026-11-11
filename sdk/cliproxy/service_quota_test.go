package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func quotaPollTestSetup(t *testing.T) (*quotaPoller, *config.Config, *coreauth.InitialQuotaPolicy, []*coreauth.Auth) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{SessionAffinity: true, QuotaAwareInitial: true}}
	s := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
	m := coreauth.NewManager(nil, s, nil)
	t.Cleanup(m.StopAutoRefresh)
	auths := []*coreauth.Auth{}
	for i := range 6 {
		auths = append(auths, &coreauth.Auth{ID: fmt.Sprint(i), Provider: "claude", RegistrationEpoch: 1, CredentialVersion: 1, Metadata: map[string]any{"access_token": "private"}})
	}
	return newQuotaPoller(), cfg, m.InitialQuotaPolicy(), auths
}

func TestQuotaPollerBoundsReadsCancelsDisableAndReplacement(t *testing.T) {
	p, cfg, policy, auths := quotaPollTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan context.Context, quotaPollConcurrency)
	p.fetch = func(ctx context.Context, _ *config.Config, _ *coreauth.Auth) ([]byte, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	p.scan(ctx, cfg, policy, auths, now)
	if len(p.active) != quotaPollConcurrency {
		t.Fatalf("active=%d", len(p.active))
	}
	for range quotaPollConcurrency {
		<-started
	}
	p.scan(ctx, cfg, policy, auths, now)
	if len(p.active) != quotaPollConcurrency {
		t.Fatal("duplicate in-flight requests")
	}
	// Replacing credentials cancels their old read; all other reads remain active.
	replacement := auths[0].Clone()
	replacement.CredentialVersion++
	auths[0] = replacement
	old := p.accounts[replacement.ID]
	p.scan(ctx, cfg, policy, auths, now)
	result := <-p.results
	if result.account != old {
		t.Fatal("wrong read cancelled")
	}
	p.complete(result, now)
	if p.accounts[replacement.ID] == old {
		t.Fatal("replacement retained old read state")
	}
	cfg.Routing.QuotaAwareInitial = false
	p.scan(ctx, cfg, policy, auths, now)
	for len(p.active) > 0 {
		p.complete(<-p.results, now)
	}
	if len(p.accounts) != 0 {
		t.Fatal("disabled poller kept accounts")
	}
	p.stop()
}

func TestQuotaPollerHourlySuccessFailureAndStaleResult(t *testing.T) {
	p, cfg, policy, auths := quotaPollTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := []byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":10,"resets_at":null}}`)
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error) { return body, nil }
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	p.scan(ctx, cfg, policy, auths[:1], now)
	result := <-p.results
	p.complete(result, now)
	if !p.accounts[auths[0].ID].next.Equal(now.Add(time.Hour)) {
		t.Fatal("not hourly")
	}
	p.scan(ctx, cfg, policy, auths[:1], now.Add(59*time.Minute))
	if len(p.active) != 0 {
		t.Fatal("early duplicate read")
	}
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error) {
		return nil, errors.New("unavailable")
	}
	p.scan(ctx, cfg, policy, auths[:1], now.Add(time.Hour))
	result = <-p.results
	p.complete(result, now.Add(time.Hour))
	if !p.accounts[auths[0].ID].next.Equal(now.Add(61 * time.Minute)) {
		t.Fatal("failure not bounded by retry interval")
	}
	p.scan(ctx, cfg, policy, auths[:1], now.Add(61*time.Minute))
	result = <-p.results
	// A late response cannot publish into a new credential state.
	auths[0] = auths[0].Clone()
	auths[0].CredentialVersion++
	p.scan(ctx, cfg, policy, nil, now)
	result.body = body
	result.err = nil
	p.complete(result, now)
	if len(p.accounts) != 0 {
		t.Fatal("late completion revived removed account")
	}
	p.stop()
}

func TestQuotaPollerRechecksInactiveAccountsAfterPrimingSlotFrees(t *testing.T) {
	p, cfg, policy, auths := quotaPollTestSetup(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error) {
		return []byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":10,"resets_at":null}}`), nil
	}
	p.scan(ctx, cfg, policy, auths[:1], now)
	p.complete(<-p.results, now)
	account := p.accounts[auths[0].ID]
	account.primingWaiting = true
	p.wakePrimingWaiters(now)
	p.scan(ctx, cfg, policy, auths[:1], now.Add(time.Second))
	if len(p.active) != 1 {
		t.Fatal("deferred inactive account still waits for hourly poll")
	}
	p.complete(<-p.results, now.Add(time.Second))
	p.stop()
}

func TestQuotaPollerPreparesIdentityBeforeUsageAndDoesNotFetchOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			p, cfg, policy, auths := quotaPollTestSetup(t)
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			p.prepare = func(_ context.Context, _ *config.Config, a *coreauth.Auth) (*coreauth.Auth, error) {
				if fail {
					return nil, errors.New("identity unavailable")
				}
				a = a.Clone()
				a.Metadata["account_uuid"] = "prepared"
				return a, nil
			}
			fetched := make(chan bool, 1)
			p.fetch = func(_ context.Context, _ *config.Config, a *coreauth.Auth) ([]byte, error) {
				fetched <- a.Metadata["account_uuid"] == "prepared"
				return []byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":10,"resets_at":null}}`), nil
			}
			p.scan(context.Background(), cfg, policy, auths[:1], now)
			result := <-p.results
			if p.complete(result, now) == fail {
				t.Fatal("preparation result ignored")
			}
			p.stop()
			if fail {
				select {
				case <-fetched:
					t.Fatal("usage fetched after preparation error")
				default:
				}
			} else if !<-fetched {
				t.Fatal("usage fetched without prepared identity")
			}
		})
	}
}
