package model_proxy

import (
	"context"
	"testing"
)

func TestUsageCorrectionPostgresDefaultsPersistenceAndIsolation(t *testing.T) {
	repo := memberPauseTestRepository(t)
	ctx := context.Background()
	initial, err := repo.GetUsageCorrection(ctx, "u", "a")
	if err != nil || initial.Enabled || initial.AllowancePercent != 10 {
		t.Fatalf("default=%+v %v", initial, err)
	}
	for _, value := range []UsageCorrectionSettings{{true, 0}, {true, 10}, {true, 20}, {true, 37}, {false, 0}} {
		if err := repo.SaveUsageCorrection(ctx, "u", "a", value); err != nil {
			t.Fatal(err)
		}
		got, err := NewRepository(repo.db).GetUsageCorrection(ctx, "u", "a")
		if err != nil || got != value {
			t.Fatalf("not persisted: %+v %v", got, err)
		}
		for _, scope := range [][2]string{{"u", "b"}, {"v", "a"}} {
			got, err := repo.GetUsageCorrection(ctx, scope[0], scope[1])
			if err != nil || got.Enabled || got.AllowancePercent != 10 {
				t.Fatalf("scope leaked: %+v %v", got, err)
			}
		}
	}
	if err := repo.SaveUsageCorrection(ctx, "u", "a", UsageCorrectionSettings{true, -1}); err == nil {
		t.Fatal("database accepted invalid percentage")
	}
}
