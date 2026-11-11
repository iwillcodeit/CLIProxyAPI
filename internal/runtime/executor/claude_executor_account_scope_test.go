package executor

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Reproduces the 429 that parked a verified account without a 5h window on
// upstream 8.0.67: a non-Fable request, weekly usage near zero, and resets pointing
// at the weekly boundary. It must stay model-scoped and must not wait for that reset.
func TestClaudeAccountRateLimitWithoutSharedExhaustionIsModelScoped(t *testing.T) {
	weekly := time.Now().Add(52 * time.Hour)
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.0")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(weekly.Unix(), 10))
	headers.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(weekly.Unix(), 10))
	headers.Set("Retry-After", strconv.Itoa(int(time.Until(weekly)/time.Second)))
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)

	err := classifyClaudeUpstreamError(http.StatusTooManyRequests, headers, body)
	var scoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &scoped) || scoped.IsCredentialScoped() {
		t.Fatalf("error %T parks the whole credential; want model-scoped", err)
	}
	var rap retryAfterProvider
	if errors.As(err, &rap) && rap.RetryAfter() != nil && *rap.RetryAfter() > 5*time.Hour+30*time.Second {
		t.Fatalf("RetryAfter = %v, want at most 5h instead of the weekly reset", *rap.RetryAfter())
	}
}
