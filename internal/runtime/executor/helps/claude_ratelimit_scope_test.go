package helps

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// A rejected unified status parks the whole credential until a reset only when a
// shared 5h or 7d window is itself exhausted. Fixtures are synthetic.
func TestClaudeRateLimitAccountScopeRequiresSharedExhaustion(t *testing.T) {
	now := time.Date(2026, 11, 11, 11, 28, 50, 0, time.UTC)
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	weekly := 52 * time.Hour
	tests := []struct {
		name        string
		headers     map[string]string
		wantAccount bool
		want        time.Duration // zero means no header-derived cooldown
	}{
		{name: "shared 7d rejected stays account-wide until reset", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-7d-Status": "rejected", "Unified-7d-Utilization": "1.0",
			"Unified-7d-Reset": unix(weekly), "Unified-Reset": unix(weekly), "Retry-After": "60",
		}, wantAccount: true, want: weekly},
		{name: "5h rejected stays account-wide until reset", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-5h-Status": "rejected", "Unified-7d-Status": "allowed",
			"Unified-5h-Reset": unix(3 * time.Hour), "Unified-Reset": unix(3 * time.Hour),
		}, wantAccount: true, want: 3 * time.Hour},
		{name: "fable or overage only without 5h headers", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-Representative-Claim": "seven_day_overage_included",
			"Unified-7d-Status": "allowed", "Unified-7d-Utilization": "0.01", "Unified-7d_oi-Status": "rejected",
			"Unified-7d_oi-Reset": unix(weekly), "Unified-Overage-Status": "rejected", "Unified-Reset": unix(weekly),
			"Retry-After": strconv.Itoa(int(weekly / time.Second)),
		}},
		// The verified no-5h account parked on 8.0.67: opus 429, weekly 0%, no overage signal.
		{name: "no 5h headers and low weekly without overage signal", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-Representative-Claim": "seven_day",
			"Unified-7d-Status": "allowed", "Unified-7d-Utilization": "0.0", "Unified-7d-Reset": unix(weekly),
			"Unified-Reset": unix(weekly), "Retry-After": strconv.Itoa(int(weekly / time.Second)),
		}, want: 5 * time.Hour},
		{name: "both windows allowed without overage signal", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-5h-Status": "allowed", "Unified-7d-Status": "allowed",
			"Unified-Reset": unix(weekly), "Retry-After": "60",
		}, want: time.Minute},
		{name: "retry-after beyond 5h is capped without shared exhaustion", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-7d-Status": "allowed", "Retry-After": strconv.Itoa(int(48 * time.Hour / time.Second)),
		}, want: 5 * time.Hour},
		{name: "unclassified rejection ignores unified reset", headers: map[string]string{
			"Unified-Status": "rejected", "Unified-Reset": unix(weekly),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			for name, value := range tt.headers {
				if name != "Retry-After" {
					name = "Anthropic-Ratelimit-" + name
				}
				headers.Set(name, value)
			}
			if got := ClaudeHeadersIndicateUnifiedRateLimitRejection(headers); got != tt.wantAccount {
				t.Fatalf("account-wide = %v, want %v", got, tt.wantAccount)
			}
			got := parseClaudeRateLimitResetWithFuzz(headers, now, 0, 0)
			if (got == nil) != (tt.want == 0) || (got != nil && *got != tt.want) {
				t.Fatalf("cooldown = %v, want %v", got, tt.want)
			}
		})
	}
}
