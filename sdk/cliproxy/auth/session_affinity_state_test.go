package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func affinityStateManager(t *testing.T) (*Manager, *SessionAffinitySelector) {
	t.Helper()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	m := NewManager(nil, selector, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"refresh_token": "credential-secret"}}
	m.auths["b"] = &Auth{ID: "b", Provider: "claude"}
	t.Cleanup(m.StopAutoRefresh)
	return m, selector
}

func TestSessionAffinityStateRestartKeepsAliasesAndPrefixWithoutSecrets(t *testing.T) {
	m, selector := affinityStateManager(t)
	selector.cache.SetAliases("b", "claude::conversation::model", "claude::child::model")
	namespace := "lcp:v1::claude::model::caller"
	turns := []cliproxysession.CanonicalTurn{{Role: "user", Parts: []cliproxysession.CanonicalPart{{Kind: "text", Value: "prompt-secret"}}}}
	sessionID := selector.matcher.Bind(namespace, turns, "b")
	expiresAt := selector.cache.snapshot()[0].ExpiresAt
	path := filepath.Join(t.TempDir(), ".session-affinity.state")
	if err := m.SaveSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "prompt-secret") || strings.Contains(string(data), "credential-secret") {
		t.Fatal("snapshot contains request content or credentials")
	}
	restarted, restored := affinityStateManager(t)
	if err := restarted.RestoreSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"conversation", "child"} {
		if id, status := restored.LookupAffinity("claude", "model", alias); id != "b" || status != "bound" {
			t.Fatalf("alias %s lost: %s/%s", alias, id, status)
		}
	}
	if !restored.cache.snapshot()[0].ExpiresAt.Equal(expiresAt) {
		t.Fatal("restart refreshed expiration")
	}
	if match, ok := restored.matcher.Match(namespace, turns); !ok || match.AuthID != "b" || match.SessionID != sessionID {
		t.Fatalf("prefix not restored: %+v", match)
	}
}

func TestSessionAffinityStateDiscardsExpiredRemovedAndDisabledCredentials(t *testing.T) {
	m, selector := affinityStateManager(t)
	selector.cache.Set("live", "a")
	selector.cache.Set("disabled", "b")
	selector.cache.Set("removed", "removed")
	path := filepath.Join(t.TempDir(), "state")
	if err := m.SaveSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var state sessionAffinityStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Bindings = append(state.Bindings, sessionBindingState{AuthID: "a", Aliases: []string{"expired"}, ExpiresAt: time.Now().Add(-time.Second)})
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, restored := affinityStateManager(t)
	restarted.auths["b"].Disabled = true
	if err := restarted.RestoreSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	if restored.cache.Len() != 1 {
		t.Fatalf("restored %d aliases, want only live credential", restored.cache.Len())
	}
	if id, _ := restored.cache.Get("live"); id != "a" {
		t.Fatal("live credential lost")
	}
}

func TestSessionAffinityStateRestoresGuardianLineage(t *testing.T) {
	m, selector := affinityStateManager(t)
	selector.cache.setSessionAliases("a", "claude", "parent", "claude::parent::m1")
	selector.cache.setSessionAliases("b", "claude", "parent", "claude::parent::m2")
	path := filepath.Join(t.TempDir(), "state")
	if err := m.SaveSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	restarted, restored := affinityStateManager(t)
	if err := restarted.RestoreSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	providers := map[string]struct{}{"claude": {}}
	if id := restored.cache.latestSessionAuth("parent", providers); id != "b" {
		t.Fatalf("latest parent auth = %q, want b", id)
	}
	restored.cache.setSessionAliases("a", "claude", "parent", "claude::parent::m3")
	if id := restored.cache.latestSessionAuth("parent", providers); id != "a" {
		t.Fatalf("new selection after restore = %q, want a", id)
	}
}

func TestSessionAffinityStateRejectsUnsafeFilesWithoutChangingMemory(t *testing.T) {
	for _, scenario := range []string{"corrupt", "version", "permissions", "symlink", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			m, selector := affinityStateManager(t)
			selector.cache.Set("existing", "a")
			path := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt":
				_ = os.WriteFile(path, []byte("{"), 0o600)
			case "version":
				_ = os.WriteFile(path, []byte(`{"version":99}`), 0o600)
			case "permissions":
				_ = os.Chmod(path, 0o644)
			case "symlink":
				target := path + "-target"
				_ = os.Rename(path, target)
				if err := os.Symlink(target, path); err != nil {
					t.Skip(err)
				}
				if err := m.SaveSessionAffinity(path); err == nil {
					t.Fatal("save followed a symlink")
				}
			case "oversized":
				if err := os.Truncate(path, maxAffinityStateBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.RestoreSessionAffinity(path); err == nil {
				t.Fatal("unsafe state accepted")
			}
			if id, ok := selector.cache.Get("existing"); !ok || id != "a" {
				t.Fatal("bad file altered live bindings")
			}
		})
	}
}

func TestManagerSelectorChangeKeepsAffinityAndLateResults(t *testing.T) {
	m, original := affinityStateManager(t)
	opts := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"conversation"}}}
	if _, err := original.Pick(context.Background(), "claude", "model", opts, []*Auth{m.auths["b"]}); err != nil {
		t.Fatal(err)
	}
	replacement := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Minute})
	m.SetSelector(replacement)
	original.Stop() // Repeated cleanup must not stop the replacement's shared state.
	got, err := replacement.Pick(context.Background(), "claude", "model", opts, []*Auth{m.auths["a"], m.auths["b"]})
	if err != nil || got.ID != "b" {
		t.Fatalf("hot reload lost affinity: %v/%v", got, err)
	}
	original.OnResult(Result{AuthID: "b", Provider: "claude", Model: "model", Options: opts, Error: &Error{HTTPStatus: 401}})
	if id, status := replacement.LookupAffinity("claude", "model", "conversation"); id != "" || status != "unbound" {
		t.Fatalf("late result did not invalidate shared binding: %s/%s", id, status)
	}
}

func TestSessionAffinityCheckpointDuringConcurrentRoutingChanges(t *testing.T) {
	m, selector := affinityStateManager(t)
	path := filepath.Join(t.TempDir(), "state")
	var workers sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				switch worker {
				case 0:
					selector.cache.SetAliases("a", "session", "parent")
				case 1:
					m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
				case 2:
					if err := m.SaveSessionAffinity(path); err != nil {
						t.Error(err)
					}
				}
			}
		}(worker)
	}
	workers.Wait()
	if err := m.SaveSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	restarted, restored := affinityStateManager(t)
	if err := restarted.RestoreSessionAffinity(path); err != nil || restored.cache.Len() != 2 {
		t.Fatalf("concurrent checkpoint lost state: %v, %d aliases", err, restored.cache.Len())
	}
	m.SetSelector(&FillFirstSelector{})
	if err := m.SaveSessionAffinity(path); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RestoreSessionAffinity(path); err != nil || restored.cache.Len() != 0 {
		t.Fatal("disabled affinity revived old bindings")
	}
}
