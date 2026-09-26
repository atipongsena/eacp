package logging

import (
	"sync"
	"time"
)

// maxTemporary bounds the temporary values a set keeps; the oldest go first.
const maxTemporary = 10000

// SecretSet is the set of values a logger redacts. It may grow after the
// logger is built: a value added later is redacted from then on. Temporary
// values (short-lived credentials) are kept until their time passes.
type SecretSet struct {
	mu        sync.RWMutex
	permanent []string
	temporary []temporarySecret // in insertion order
	now       func() time.Time
}

type temporarySecret struct {
	value string
	until time.Time
}

// NewSecretSet returns a set holding values permanently.
func NewSecretSet(values ...string) *SecretSet {
	s := &SecretSet{now: time.Now}
	s.AddPermanent(values...)
	return s
}

// AddPermanent redacts values for the life of the set. Empty values are ignored.
func (s *SecretSet) AddPermanent(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if v != "" {
			s.permanent = append(s.permanent, v)
		}
	}
}

// Add redacts value until the given time, dropping expired values and, past
// maxTemporary, the oldest ones. An empty value is ignored.
func (s *SecretSet) Add(value string, until time.Time) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	kept := s.temporary[:0]
	for _, t := range s.temporary {
		if t.until.After(now) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, temporarySecret{value, until})
	if over := len(kept) - maxTemporary; over > 0 {
		kept = append(kept[:0], kept[over:]...)
	}
	s.temporary = kept
}

// Values returns the permanent values, then the unexpired temporary ones.
func (s *SecretSet) Values() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	out := make([]string, 0, len(s.permanent)+len(s.temporary))
	out = append(out, s.permanent...)
	for _, t := range s.temporary {
		if t.until.After(now) {
			out = append(out, t.value)
		}
	}
	return out
}
