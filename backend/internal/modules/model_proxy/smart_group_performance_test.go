package model_proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkSmartGroupRequestParsing(b *testing.B) {
	for _, size := range []int{128 << 10, 1 << 20} {
		body, _ := json.Marshal(map[string]any{"model": "alias", "input": strings.Repeat("long context ", size/13), "stream": true})
		for _, shared := range []bool{false, true} {
			name := "repeated-parsing"
			if shared {
				name = "shared-parsing"
			}
			if size == 1<<20 {
				name += "/1MiB"
			} else {
				name += "/128KiB"
			}
			b.Run(name, func(b *testing.B) {
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(string(body))), 2<<20)
				if err != nil {
					b.Fatal(err)
				}
				defer replay.Close()
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				group := SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100, Keywords: []string{"premium"}}
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					replay.discardObject()
					if shared {
						_, _ = replay.Object()
					}
					_, _ = replay.ModelID()
					_, _, _, _ = requestPolicyFactsWithText(request, replay)
					if !shared {
						replay.discardObject()
					}
					if (&Service{}).serveGroupLocalReply(httptest.NewRecorder(), request, group, replay, "alias") {
						b.Fatal("long request returned locally")
					}
					if !shared {
						replay.discardObject()
					}
					_, _, _ = requestPolicyFacts(request, replay)
				}
			})
		}
	}
}

func TestSmartGroupConcurrentFullRequestPath(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var upstreamCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["model"] != "model" {
			t.Errorf("upstream model mapping: %v %v", payload, err)
		}
		upstreamCalls.Add(1)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"upstream","object":"response","model":"model","status":"completed","output":[]}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.accounts, service.limiter = repo, proxyTestAccount{}, limiter
	service.statistics = newRequestStatistics(limiter)
	ctx := context.Background()
	groupID := fmt.Sprintf("concurrent-group-%d", time.Now().UnixNano())
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, groupID)
	routeIDs := []string{route.ID}
	for i := 0; i < 3; i++ {
		other := healthTestRoute(t, client)
		if _, err := repo.db.Exec(ctx, `INSERT INTO proxy_routes(id,user_id,admin_account_id,name,site_id,group_id,group_name,upstream_key_ciphertext) VALUES($1,'user','workspace','parallel','site','1','group',$2)`, other.ID, route.UpstreamKeyCiphertext); err != nil {
			t.Fatal(err)
		}
		if err := repo.AddMember(ctx, "user", "workspace", groupID, other.ID); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceRouteModels(ctx, other.ID, []Model{{ID: "model", Object: "model"}}); err != nil {
			t.Fatal(err)
		}
		routeIDs = append(routeIDs, other.ID)
	}
	enabled := true
	mapping := map[string]string{"alias": "model"}
	for _, id := range routeIDs {
		if err := service.UpdateMemberPolicy(ctx, "user", groupID, id, UpdateMemberPolicyRequest{UseProvidedKey: &enabled, ModelMappingEnabled: &enabled, ModelMapping: &mapping}); err != nil {
			t.Fatal(err)
		}
	}
	keywords := []string{"premium"}
	if _, err := service.UpdateSmartGroup(ctx, "user", groupID, UpdateSmartGroupRequest{Keywords: &keywords, LocalReplyEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	keyID, key, hash, ciphertext, preview, err := service.newAccessKey(OwnerSmartGroup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(ctx, `INSERT INTO proxy_access_keys(id,user_id,admin_account_id,owner_type,owner_id,key_hash,key_ciphertext,key_preview) VALUES($1,'user','workspace','smart_group',$2,$3,$4,$5)`, keyID, groupID, hash, ciphertext, preview); err != nil {
		t.Fatal(err)
	}
	keywordKey, _ := keywordMatchCountKey(groupID, time.Now())
	routeKey, _ := routeRequestCountKey(time.Now())
	t.Cleanup(func() { client.Del(ctx, keywordKey); client.HDel(ctx, routeKey, routeIDs...) })
	body := fmt.Sprintf(`{"model":"alias","input":%q,"max_output_tokens":32}`, strings.Repeat("premium context ", 100))
	handler := service.PublicHandler()
	var wg sync.WaitGroup
	started := time.Now()
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"alias"`) {
					t.Errorf("concurrent response: %d %s", w.Code, w.Body.String())
				}
			}
		}()
	}
	wg.Wait()
	service.statistics.flush(ctx)
	if upstreamCalls.Load() != 320 || client.HGet(ctx, keywordKey, "premium").Val() != "320" {
		t.Fatalf("requests=%d keyword=%s", upstreamCalls.Load(), client.HGet(ctx, keywordKey, "premium").Val())
	}
	var total int64
	for _, id := range routeIDs {
		count, _ := client.HGet(ctx, routeKey, id).Int64()
		total += count
		if active, _ := limiter.Active(ctx, id); active != 0 {
			t.Fatalf("leaked route lease: %s %d", id, active)
		}
	}
	if total != 320 {
		t.Fatalf("route dispatch counts=%d", total)
	}
	t.Logf("320 requests, 32 workers, auth/policies/mapping/scheduling/statistics: %s", time.Since(started))
}

func BenchmarkSmartGroupRepository(b *testing.B) {
	repo := memberPauseTestRepository(b)
	ctx := context.Background()
	seedPauseMember(b, repo, Route{ID: "perf-base"}, "perf-group")
	_, err := repo.db.Exec(ctx, `INSERT INTO proxy_routes(id,user_id,admin_account_id,name,site_id,group_id,group_name)
SELECT 'perf-'||n,'user','workspace','perf route','site','1','group' FROM generate_series(1,29) n;
INSERT INTO proxy_smart_group_members(smart_group_id,route_id) SELECT 'perf-group',id FROM proxy_routes WHERE id<>'perf-base';
INSERT INTO proxy_route_models(route_id,model_id,model_json) SELECT id,'model','{"id":"model","object":"model"}'::jsonb FROM proxy_routes WHERE id<>'perf-base';`)
	if err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"full-admin-auth", "settings-auth", "full-candidates", "lean-candidates"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				switch name {
				case "full-admin-auth":
					_, err = repo.GetSmartGroup(ctx, "perf-group")
				case "settings-auth":
					_, err = repo.GetSmartGroupConfig(ctx, "perf-group")
				case "full-candidates":
					_, err = repo.ListGroupRoutes(ctx, "perf-group", false, "model")
				case "lean-candidates":
					_, err = repo.ListGroupRoutes(ctx, "perf-group", true, "model")
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestSmartGroupLightweightReadsPreserveLivePolicies(t *testing.T) {
	repo := memberPauseTestRepository(t)
	ctx := context.Background()
	seedPauseMember(t, repo, Route{ID: "live-route"}, "live-group")
	text, limit, enabled := "自定义回复", 27, true
	svc := &Service{repository: repo, accounts: proxyTestAccount{}}
	if _, err := svc.UpdateSmartGroup(ctx, "user", "live-group", UpdateSmartGroupRequest{LocalReplyText: &text, LocalReplyMaxOutputTokens: &limit, LocalReplyEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if _, err := svc.UpdateSmartGroup(ctx, "user", "live-group", UpdateSmartGroupRequest{Name: &name}); err != nil {
		t.Fatal(err)
	}
	config, err := repo.GetSmartGroupConfig(ctx, "live-group")
	if err != nil || config.LocalReplyText != text || config.LocalReplyMaxOutputTokens != limit || !config.LocalReplyEnabled || len(config.Members) != 0 || len(config.Models) != 0 || len(config.CustomKeys) != 0 {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	routes, err := repo.ListGroupRoutes(ctx, "live-group", true, "model")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "live-group", "live-route", UpdateMemberPolicyRequest{SchedulingPaused: &enabled}); err != nil {
		t.Fatal(err)
	}
	routes, err = repo.ListGroupRoutes(ctx, "live-group", true, "model")
	if err != nil || len(routes) != 0 {
		t.Fatalf("pause did not take immediate effect: %v %v", routes, err)
	}
}

func TestSharedRequestParsingPreservesReplayAndRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"model":"alias","input":"hi"}`, `{"model":"alias"} {"extra":true}`} {
		replay, err := newReplayBody(io.NopCloser(strings.NewReader(body)), 1024)
		if err != nil {
			t.Fatal(err)
		}
		_, err = replay.Object()
		if (err == nil) != !strings.Contains(body, "extra") {
			t.Fatalf("trailing JSON validation: %v", err)
		}
		reader, _ := replay.Open()
		raw, _ := io.ReadAll(reader)
		reader.Close()
		if string(raw) != body {
			t.Fatal("shared parsing changed forwarded bytes")
		}
		replay.discardObject()
		if replay.object != nil || replay.objectRead {
			t.Fatal("parsed context retained before upstream streaming")
		}
		replay.Close()
	}
}
