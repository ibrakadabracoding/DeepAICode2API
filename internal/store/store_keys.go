package store

import (
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Keys ---

// Keys returns a snapshot of keys.
//
// Elements must be copied one by one: after receiving the slice, the caller reads fields outside the lock (the console's safeKeyList),
// and ValidKey accumulates LastUsed / Requests in place on every data-plane request. Returning live
// pointers from the account pool would turn "console open + traffic flowing" into a real data race (reproducible with -race).
func (s *Store) Keys() []*APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*APIKey, len(s.st.Keys))
	for i, k := range s.st.Keys {
		c := *k
		out[i] = &c
	}
	return out
}

// AddKey adds a key (grouped into default when group is empty).
//
// The return value is a copy: the caller usually reads the Key field right after to return it to the user, and should not hold a live
// pointer from the account pool (same reason as Keys).
func (s *Store) AddKey(name, group string) *APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	group = strings.TrimSpace(group)
	if group == "" {
		group = "default"
	}
	k := &APIKey{
		Key:       "sk-" + deepai.RandomHex(24),
		Name:      name,
		Group:     group,
		Enabled:   true,
		CreatedAt: time.Now().Unix(),
	}
	s.st.Keys = append(s.st.Keys, k)
	c := *k
	return &c
}

// KeyGroup returns the account pool group bound to a gateway key (returns an empty string for an unknown key).
func (s *Store) KeyGroup(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			return k.Group
		}
	}
	return ""
}

// SetKeyGroup changes the account pool group bound to a key (an empty string is equivalent to default).
//
// Used for operational adjustments: for example, after clearing a stress-test group, rebind the key back to default,
// to avoid "no available accounts in the group". Returns whether the key was found.
func (s *Store) SetKeyGroup(key, group string) bool {
	group = strings.TrimSpace(group)
	if group == "" {
		group = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			k.Group = group
			return true
		}
	}
	return false
}

// DeleteKey deletes a key.
func (s *Store) DeleteKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.st.Keys {
		if k.Key == key {
			s.st.Keys = append(s.st.Keys[:i], s.st.Keys[i+1:]...)
			return true
		}
	}
	return false
}

// ToggleKey enables/disables a key.
func (s *Store) ToggleKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			k.Enabled = !k.Enabled
			return true
		}
	}
	return false
}

// ValidKey validates a key and counts it.
func (s *Store) ValidKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key && k.Enabled {
			k.LastUsed = time.Now().Unix()
			k.Requests++
			return true
		}
	}
	return false
}

// HasKeys reports whether any key has been issued.
func (s *Store) HasKeys() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.st.Keys) > 0
}
