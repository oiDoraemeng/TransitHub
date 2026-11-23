package model_proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"transithub/backend/internal/database/migrations"
)

func TestMemberMediaInputPersistsAndIsolatesGroups(t *testing.T) {
	repo := memberPauseTestRepository(t)
	route := Route{ID: "media-member"}
	seedPauseMember(t, repo, route, "a", "b")
	service := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	for _, enabled := range []bool{true, false} {
		if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{MediaInputEnabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		reloaded := NewRepository(repo.db)
		for _, group := range []string{"a", "b"} {
			for _, model := range []string{"", "model"} {
				rows, err := reloaded.ListGroupRoutes(ctx, group, true, model)
				if err != nil || len(rows) != 1 || rows[0].MediaInputEnabled != (group == "a" && enabled) {
					t.Fatalf("group=%s model=%s enabled=%v rows=%+v err=%v", group, model, enabled, rows, err)
				}
			}
		}
		priority := 2
		if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{Priority: &priority}); err != nil {
			t.Fatal(err)
		}
		rows, err := reloaded.ListGroupRoutes(ctx, "a", true, "model")
		if err != nil || len(rows) != 1 || rows[0].MediaInputEnabled != enabled {
			t.Fatalf("unrelated update changed media flag: %+v %v", rows, err)
		}
		if err := service.UpdateMemberPolicy(ctx, "other-user", "a", route.ID, UpdateMemberPolicyRequest{MediaInputEnabled: &enabled}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized update: %v", err)
		}
	}
	// The once-only migration must never turn a user-disabled Ultra member back on.
	if _, err := repo.db.Exec(ctx, `UPDATE proxy_routes SET name='gemini ultra' WHERE id=$1`, route.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, repo.db); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListGroupRoutes(ctx, "a", false, "")
	if err != nil || len(rows) != 1 || rows[0].MediaInputEnabled {
		t.Fatalf("restart reset user preference: %+v %v", rows, err)
	}
	direct, err := repo.GetRoute(ctx, route.ID)
	if err != nil || direct == nil || direct.MediaInputEnabled {
		t.Fatalf("member flag leaked into direct route: %+v %v", direct, err)
	}
}

func TestMemberMediaInputMigrationPreservesExistingDestination(t *testing.T) {
	repo := memberPauseTestRepository(t)
	ctx := context.Background()
	seedPauseMember(t, repo, Route{ID: "existing-ultra"}, "existing-group")
	seedPauseMember(t, repo, Route{ID: "other-route"})
	if err := repo.AddMember(ctx, "user", "workspace", "existing-group", "other-route"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(ctx, `UPDATE proxy_routes SET name=' Gemini   Ultra ' WHERE id='existing-ultra'`); err != nil {
		t.Fatal(err)
	}
	// Recreate the schema immediately before this migration in an isolated DB.
	if _, err := repo.db.Exec(ctx, `ALTER TABLE proxy_smart_group_members DROP COLUMN media_input_enabled`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(ctx, `DELETE FROM schema_migrations WHERE version='000041_add_member_media_input'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, repo.db); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListGroupRoutes(ctx, "existing-group", false, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("migration rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.MediaInputEnabled != (row.ID == "existing-ultra") {
			t.Fatalf("wrong migrated destination: %+v", row)
		}
	}
}
