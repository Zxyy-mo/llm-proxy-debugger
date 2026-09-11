package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDurableSettingIsCommittedBeforeReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.SetSettingDurable("registration", map[string]string{"key": "accepted"}); err != nil {
		t.Fatal(err)
	}
	// Read through a separate SQLite connection without closing/flushing the
	// Store. A successful helper return is itself the durability boundary.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload []byte
	if err := db.QueryRow("SELECT payload FROM gateway_state WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var state diskState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	if err := json.Unmarshal(state.Settings["registration"], &saved); err != nil || saved["key"] != "accepted" {
		t.Fatalf("successful registration was not committed: %s %v", state.Settings["registration"], err)
	}
}

func TestDurableSettingFailureRollsBackBeforeLaterFlush(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.SetSettingDurable("replays", []string{"previous-key"}); err != nil {
		t.Fatal(err)
	}
	p := s.persistence
	p.mu.Lock()
	_, err = p.db.Exec("CREATE TRIGGER reject_state_save BEFORE UPDATE ON gateway_state BEGIN SELECT RAISE(ABORT, 'forced state save failure'); END")
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var restored sync.Once
	recoverStorage := func() {
		restored.Do(func() {
			p.mu.Lock()
			_, err := p.db.Exec("DROP TRIGGER reject_state_save")
			p.mu.Unlock()
			if err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(recoverStorage)
	s.SetSetting("unrelated", "retain this concurrent metadata")
	lastSave := s.PersistenceStatus()["last_saved_at"]
	if err := s.SetSettingDurable("replays", []string{"rejected-key"}); err == nil || !strings.Contains(err.Error(), "forced state save failure") {
		t.Fatalf("write failure was not returned: %v", err)
	}
	var keys []string
	if !s.Setting("replays", &keys) || len(keys) != 1 || keys[0] != "previous-key" {
		t.Fatalf("failed save replaced the prior in-memory key: %+v", keys)
	}
	if err := s.SetSettingDurable("new-setting", "rejected-candidate"); err == nil {
		t.Fatal("new setting should also fail while writes are rejected")
	}
	var absent string
	if s.Setting("new-setting", &absent) {
		t.Fatal("failed save left a previously absent setting in memory")
	}
	status := s.PersistenceStatus()
	if status["error"] == nil || status["last_saved_at"] != lastSave {
		t.Fatalf("save failure was hidden or advanced the saved timestamp: %+v", status)
	}
	recoverStorage()
	// Flush is the same path used by the background worker. It must commit the
	// rolled-back state, not resurrect either rejected candidate.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := p.db.QueryRow("SELECT payload FROM gateway_state WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "rejected-") || !strings.Contains(string(payload), "previous-key") || !strings.Contains(string(payload), "retain this concurrent metadata") {
		t.Fatalf("later flush lost retained settings or saved a rejected value: %s", payload)
	}
	if err := s.SetSettingDurable("replays", []string{"previous-key", "retried-key"}); err != nil {
		t.Fatalf("storage did not recover: %v", err)
	}
}

func TestDurableSettingRejectsClosedStoreAndInvalidEncoding(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		name := "memory"
		path := "-"
		if persistent {
			name, path = "sqlite", filepath.Join(t.TempDir(), "gateway.db")
		}
		t.Run(name, func(t *testing.T) {
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.SetSettingDurable("key", "previous"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSettingDurable("key", make(chan int)); err == nil {
				t.Fatal("invalid JSON value was silently accepted")
			}
			var value string
			if !s.Setting("key", &value) || value != "previous" {
				t.Fatal("encoding failure changed the setting")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if persistent {
				if err := s.SetSettingDurable("key", "after-close"); err == nil {
					t.Fatal("closed database accepted a new durable setting")
				}
				if !s.Setting("key", &value) || value != "previous" || s.PersistenceStatus()["error"] == nil {
					t.Fatal("closed-database failure changed state or was hidden")
				}
			} else if s.PersistenceStatus()["enabled"] != false {
				t.Fatal("memory-only setting claimed persistent storage")
			}
		})
	}
}
