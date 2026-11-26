package model_proxy

import (
	"context"
	"testing"
)

func TestUsageCorrectionPostgresGroupDefaultsPersistenceAndIsolation(t *testing.T) {
	repo := memberPauseTestRepository(t)
	ctx := context.Background()
	seedPauseMember(t, repo, Route{ID: "route"}, "a", "b")
	service := &Service{repository: repo, accounts: proxyTestAccount{}}
	for _, id := range []string{"a", "b"} {
		group, err := repo.GetSmartGroupConfig(ctx, id)
		if err != nil || group.UsageCorrectionEnabled || group.UsageCorrectionAllowancePercent != 40 {
			t.Fatalf("default=%+v %v", group, err)
		}
	}
	enabled := true
	for _, percent := range []int{0, 10, 20, 37, 1000} {
		_, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{UsageCorrectionEnabled: &enabled, UsageCorrectionAllowancePercent: &percent})
		if err != nil {
			t.Fatal(err)
		}
		reloaded := NewRepository(repo.db)
		group, err := reloaded.GetSmartGroupConfig(ctx, "a")
		if err != nil || !group.UsageCorrectionEnabled || group.UsageCorrectionAllowancePercent != percent {
			t.Fatalf("persistence=%+v %v", group, err)
		}
		groups, err := reloaded.ListSmartGroups(ctx, "user", "workspace")
		if err != nil || len(groups) != 2 {
			t.Fatalf("list=%+v %v", groups, err)
		}
		other, err := reloaded.GetSmartGroupConfig(ctx, "b")
		if err != nil || other.UsageCorrectionEnabled || other.UsageCorrectionAllowancePercent != 40 {
			t.Fatalf("leaked to other group: %+v %v", other, err)
		}
		// An unrelated partial update must preserve the correction settings.
		name := "renamed"
		unchanged, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{Name: &name})
		if err != nil || !unchanged.UsageCorrectionEnabled || unchanged.UsageCorrectionAllowancePercent != percent {
			t.Fatalf("partial update lost settings: %+v %v", unchanged, err)
		}
	}
	disabled := false
	if _, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{UsageCorrectionEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int{-1, 1001} {
		if _, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{UsageCorrectionAllowancePercent: &bad}); err == nil {
			t.Fatal("invalid percentage accepted")
		}
	}
	zero := 0
	if _, err := service.UpdateSmartGroup(ctx, "another-user", "a", UpdateSmartGroupRequest{UsageCorrectionAllowancePercent: &zero}); err == nil {
		t.Fatal("cross-user update accepted")
	}
	if _, err := repo.db.Exec(ctx, `UPDATE proxy_smart_groups SET usage_correction_allowance_percent=-1 WHERE id='a'`); err == nil {
		t.Fatal("database accepted invalid percentage")
	}
	created := SmartGroup{ID: "c", UserID: "user", AdminAccountID: "workspace", Name: "new", Enabled: true, Keywords: []string{}, LocalReplyMinTokens: 100, LocalReplyMaxOutputTokens: 20, LocalReplyText: "OK", UsageCorrectionEnabled: true, UsageCorrectionAllowancePercent: 0}
	if err := repo.CreateSmartGroup(ctx, created, []string{"route"}, "key-c", "hash-c", "cipher-c", "preview-c"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetSmartGroupConfig(ctx, "c")
	if err != nil || !got.UsageCorrectionEnabled || got.UsageCorrectionAllowancePercent != 0 {
		t.Fatalf("create lost settings: %+v %v", got, err)
	}
}
