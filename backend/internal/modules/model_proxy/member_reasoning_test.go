package model_proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMemberReasoningEffortPersistsAndIsolatesGroups(t *testing.T) {
	repo := memberPauseTestRepository(t)
	seedPauseMember(t, repo, Route{ID: "reasoning-route"}, "a", "b")
	svc := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", ""} {
		if err := svc.UpdateMemberPolicy(ctx, "user", "a", "reasoning-route", UpdateMemberPolicyRequest{ReasoningEffort: &effort}); err != nil {
			t.Fatal(err)
		}
		priority := 7
		if err := svc.UpdateMemberPolicy(ctx, "user", "a", "reasoning-route", UpdateMemberPolicyRequest{Priority: &priority}); err != nil {
			t.Fatal(err)
		}
		for _, enabledOnly := range []bool{false, true} {
			model := ""
			if enabledOnly {
				model = "model"
			}
			members, err := NewRepository(repo.db).ListGroupRoutes(ctx, "a", enabledOnly, model)
			if err != nil || len(members) != 1 || members[0].ReasoningEffort != effort || members[0].Priority != priority {
				t.Fatalf("persisted member=%+v err=%v want effort=%q", members, err, effort)
			}
		}
		other, err := repo.ListGroupRoutes(ctx, "b", true, "model")
		if err != nil || len(other) != 1 || other[0].ReasoningEffort != "" {
			t.Fatalf("other group changed: %+v %v", other, err)
		}
	}
	effort := "low"
	if err := svc.UpdateMemberPolicy(ctx, "other-user", "a", "reasoning-route", UpdateMemberPolicyRequest{ReasoningEffort: &effort}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("member authorization bypass: %v", err)
	}
}

func TestMemberReasoningEffortRejectsUnsupportedValues(t *testing.T) {
	svc := &Service{accounts: proxyTestAccount{}}
	for _, value := range []string{"auto", "none", "minimal", "ultra"} {
		err := svc.UpdateMemberPolicy(context.Background(), "user", "a", "route", UpdateMemberPolicyRequest{ReasoningEffort: &value})
		var invalid *requestError
		if !errors.As(err, &invalid) || invalid.Status != 400 {
			t.Fatalf("invalid effort=%q err=%v", value, err)
		}
	}
	got, err := normalizeReasoningEffort(" XHIGH ")
	if err != nil || got != "xhigh" {
		t.Fatalf("normalize=%q %v", got, err)
	}
}
