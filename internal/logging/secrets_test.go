package logging

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestALoggerRedactsASecretAddedAfterItWasBuilt(t *testing.T) {
	set := NewSecretSet()
	var buf bytes.Buffer
	log := NewWithSet(&buf, slog.LevelDebug, "json", set)
	set.Add(canary+"-minted", time.Now().Add(time.Hour))
	set.AddPermanent(canary + "-static")
	log.Info("call", "reference", "PO-"+canary+"-minted", "err", fmt.Errorf("bad %s", canary+"-static"))
	if strings.Contains(buf.String(), canary) {
		t.Fatalf("a secret added after the logger was built leaked: %s", buf.String())
	}
}

func TestTemporarySecretsExpireAndAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	set := NewSecretSet("perm")
	set.now = func() time.Time { return now }
	set.Add("short", now.Add(time.Minute))
	set.Add("long", now.Add(time.Hour))
	now = now.Add(2 * time.Minute)
	if got := strings.Join(set.Values(), ","); got != "perm,long" {
		t.Fatalf("values = %s, want perm,long", got)
	}
	for i := range maxTemporary + 5 {
		set.Add(fmt.Sprintf("v%d", i), now.Add(time.Hour))
	}
	v := set.Values()
	if len(v) != 1+maxTemporary {
		t.Fatalf("%d values, want %d", len(v), 1+maxTemporary)
	}
	if v[1] == "long" || v[1] == "v0" {
		t.Fatalf("the oldest temporary values were not dropped first: %v", v[:3])
	}
	set.Add("", now.Add(time.Hour))
	set.AddPermanent("")
	if n := len(set.Values()); n != 1+maxTemporary {
		t.Fatal("an empty value was added")
	}
}

func TestAddingAPermanentValueAgainKeepsOneEntry(t *testing.T) {
	s := NewSecretSet("a")
	s.AddPermanent("a", "b", "b")
	if got := s.Values(); len(got) != 2 {
		t.Fatalf("Values() = %v, want a and b once each", got)
	}
}
