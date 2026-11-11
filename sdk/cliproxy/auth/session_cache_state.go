package auth

import (
	"container/list"
	"time"
)

type sessionBindingState struct {
	AuthID    string    `json:"auth_id"`
	Aliases   []string  `json:"aliases"`
	ExpiresAt time.Time `json:"expires_at"`
	// Selection lineage lets guardian requests inherit the newest parent binding.
	SessionID string `json:"session_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Sequence  uint64 `json:"sequence,omitempty"`
}

func (c *SessionCache) snapshot() []sessionBindingState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []sessionBindingState
	now := time.Now()
	for element := c.evictionOrder.Front(); element != nil; element = element.Next() {
		g := c.groups[element.Value.(string)]
		if now.Before(g.expiresAt) {
			result = append(result, sessionBindingState{g.authID, append([]string(nil), g.aliases...), g.expiresAt,
				g.binding.sessionID, g.binding.provider, g.binding.sequence})
		}
	}
	return result
}

func (c *SessionCache) restore(groups []sessionBindingState, acceptAuth func(string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]sessionEntry)
	c.groups = make(map[string]sessionEntry)
	c.evictionOrder = list.New()
	c.evictionElements = make(map[string]*list.Element)
	c.sessionGroups = make(map[string]map[string]struct{})
	c.bindingSequence = 0
	now := time.Now()
	for _, g := range groups {
		if !now.Before(g.ExpiresAt) || !acceptAuth(g.AuthID) || len(g.Aliases) == 0 || len(g.Aliases) > maxStableSessionAliases+1 {
			continue
		}
		valid := true
		for _, alias := range g.Aliases {
			if alias == "" || len(alias) > 8192 {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		aliases := compactSessionAliases(mergeSessionAliases(nil, g.Aliases...))
		var previous []sessionEntry
		for _, alias := range aliases {
			if entry, ok := c.entries[alias]; ok {
				previous = append(previous, entry)
			}
		}
		var binding sessionBinding
		if g.SessionID != "" && len(g.SessionID) <= 8192 && g.Provider != "" && len(g.Provider) <= 256 && g.Sequence != 0 {
			binding = sessionBinding{sessionID: g.SessionID, provider: g.Provider, sequence: g.Sequence}
			c.bindingSequence = max(c.bindingSequence, g.Sequence)
		}
		c.replaceAliasGroupsLocked(g.AuthID, g.ExpiresAt, aliases, binding, previous...)
	}
}
