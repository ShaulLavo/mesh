package serve

import (
	"strings"
	"testing"
	"time"
)

func keptRecipeService(idle time.Duration) Service {
	return Service{Name: "ai", Kind: Proxy, Target: "8317", Demand: &Demand{Command: "./run", Cwd: "/srv/proxy", Idle: idle, KeepRunning: true}}
}

func TestNormalizeKeptRecipe(t *testing.T) {
	normalized, err := Normalize(keptRecipeService(0))
	if err != nil {
		t.Fatal(err)
	}
	if demand := normalized.Demand; !demand.KeepRunning || demand.Idle != 0 || demand.ReadyTimeout != DefaultReadyTimeout {
		t.Fatalf("kept recipe normalized to %+v, want no idle window and the default ready timeout", demand)
	}
	if _, err := Normalize(keptRecipeService(time.Minute)); err == nil || !strings.Contains(err.Error(), "keeps running") {
		t.Fatalf("a kept recipe with an idle window = %v, want it refused", err)
	}
	finite := keptRecipeService(0)
	finite.Demand.KeepRunning = false
	normalized, err = Normalize(finite)
	if err != nil || normalized.Demand.Idle != DefaultIdle {
		t.Fatalf("a finite recipe with no idle window = %+v, %v; want the default", normalized.Demand, err)
	}
	kept, _ := Normalize(keptRecipeService(0))
	if kept.Equal(normalized) || normalized.Equal(kept) {
		t.Fatal("equality does not tell a kept recipe from a finite one")
	}
	if !kept.Demand.Kept() || normalized.Demand.Kept() || (*Demand)(nil).Kept() {
		t.Fatal("Kept misreports a lifetime")
	}
}
