package model_proxy

import (
	"context"
	"encoding/base64"
	"testing"
)

func TestSmartGroupMemberKeyUpdatesSharedRouteKey(t *testing.T) {
	repo := memberPauseTestRepository(t)
	cipher, err := NewKeyCipher(base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oldRouteKey, err := cipher.Encrypt("sk-old-route")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: repo, accounts: proxyTestAccount{}, cipher: cipher}
	route := Route{ID: "shared-key-route", UpstreamKeyCiphertext: oldRouteKey}
	seedPauseMember(t, repo, route, "group-a", "group-b")
	if _, err := repo.db.Exec(ctx, `UPDATE proxy_routes SET enabled=false WHERE id=$1`, route.ID); err != nil {
		t.Fatal(err)
	}

	for _, groupID := range []string{"group-a", "group-b"} {
		legacyKey, err := cipher.Encrypt("sk-legacy-" + groupID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(ctx, `
			UPDATE proxy_smart_group_members
			SET use_upstream_key=true,upstream_key_ciphertext=$3,upstream_key_preview=$4
			WHERE smart_group_id=$1 AND route_id=$2
		`, groupID, route.ID, legacyKey, keyPreview("sk-legacy-"+groupID)); err != nil {
			t.Fatal(err)
		}
	}

	useProvidedKey := true
	if err := service.UpdateMemberPolicy(ctx, "user", "group-a", route.ID, UpdateMemberPolicyRequest{
		UseProvidedKey: &useProvidedKey,
		UpstreamKey:    "sk-shared-route-key",
	}); err != nil {
		t.Fatal(err)
	}

	updatedRoute, err := repo.GetRoute(ctx, route.ID)
	if err != nil || updatedRoute == nil {
		t.Fatalf("route=%+v err=%v", updatedRoute, err)
	}
	decrypted, err := cipher.Decrypt(updatedRoute.UpstreamKeyCiphertext)
	if err != nil || decrypted != "sk-shared-route-key" || updatedRoute.UpstreamKeyPreview != keyPreview(decrypted) {
		t.Fatalf("route key=%q preview=%q err=%v", decrypted, updatedRoute.UpstreamKeyPreview, err)
	}

	for _, groupID := range []string{"group-a", "group-b"} {
		members, err := repo.ListGroupRoutes(ctx, groupID, false, "")
		if err != nil || len(members) != 1 {
			t.Fatalf("group=%s members=%+v err=%v", groupID, members, err)
		}
		member := members[0]
		if !member.UseProvidedKey || !member.RouteProvidedKey || member.MemberProvidedKey ||
			member.UpstreamKeyCiphertext != updatedRoute.UpstreamKeyCiphertext || member.UpstreamKeyPreview != updatedRoute.UpstreamKeyPreview {
			t.Fatalf("group=%s still has an independent member key: %+v", groupID, member)
		}
	}
}
