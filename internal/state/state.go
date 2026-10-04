// Package state records which Herdr profiles this plugin created. herdr
// machine list has no ownership marker, so this file is what keeps sync from
// ever removing a hand-registered machine.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type Profile struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	AddedAt   time.Time `json:"added_at"`
}

type State struct {
	// Profiles is keyed by SSH alias.
	Profiles map[string]Profile `json:"profiles"`
}

func Load(path string) (*State, error) {
	s := &State{Profiles: map[string]Profile{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Profile{}
	}
	return s, nil
}

func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Owns reports whether a Herdr profile ID was created by this plugin.
func (s *State) Owns(id string) bool {
	for _, p := range s.Profiles {
		if p.ID == id {
			return true
		}
	}
	return false
}

// Forget drops every record of a profile ID.
func (s *State) Forget(id string) {
	for alias, p := range s.Profiles {
		if p.ID == id {
			delete(s.Profiles, alias)
		}
	}
}
