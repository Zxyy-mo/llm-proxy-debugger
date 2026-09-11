package store

import (
	"encoding/json"
	"errors"
	"time"
)

// SetSettingDurable atomically updates a setting and commits the metadata
// snapshot before returning. Failed saves leave the previous in-memory setting
// intact, so a later background flush cannot save a rejected registration.
// With memory-only storage it applies the setting without a durability promise.
func (s *Store) SetSettingDurable(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p := s.persistence
	if p == nil {
		s.Lock()
		s.settings[key] = raw
		s.Unlock()
		return nil
	}

	// Match Flush's lock order. Holding both locks through the commit prevents
	// readers and background saves from observing a candidate that may roll back.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.saveLocked(nil, nil)
	}
	s.Lock()
	defer s.Unlock()
	previous, existed := s.settings[key]
	s.settings[key] = raw
	payload, err := s.snapshotJSONLocked()
	if err = p.saveLocked(payload, err); err != nil {
		if existed {
			s.settings[key] = previous
		} else {
			delete(s.settings, key)
		}
	}
	return err
}

// saveLocked commits an already encoded snapshot and records the result.
// The caller must hold persistence.mu to serialize every SQLite state write.
func (p *persistence) saveLocked(payload []byte, err error) error {
	if p.closed {
		err = errors.New("history database closed")
	}
	if err == nil {
		_, err = p.db.Exec("INSERT INTO gateway_state(id,payload) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload", payload)
	}
	p.err = err
	if err == nil {
		p.lastSave = time.Now().UTC()
	}
	return err
}
