package store

import (
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Model catalog ---

// Models returns a snapshot of the model catalog.
func (s *Store) Models() []deepai.Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]deepai.Model, len(s.st.Models))
	copy(out, s.st.Models)
	return out
}

// DefaultModel returns the current default model.
func (s *Store) DefaultModel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.DefaultModel
}

// ModelsUpdated returns the catalog update time.
func (s *Store) ModelsUpdated() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.ModelsUpdated
}

// SetModels updates the model catalog cache.
func (s *Store) SetModels(models []deepai.Model, def string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Models = models
	if def != "" {
		s.st.DefaultModel = def
	} else if len(models) > 0 && s.st.DefaultModel == "" {
		s.st.DefaultModel = models[0].ID
	}
	s.st.ModelsUpdated = time.Now().Unix()
}

// SetMachineID records this machine's fingerprint.
func (s *Store) SetMachineID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.MachineID = id
}

// MachineID returns the recorded machine fingerprint.
func (s *Store) MachineID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.MachineID
}
