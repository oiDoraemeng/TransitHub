package model_proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestEmbeddingsReturnLocallyWithoutRouting(t *testing.T) {
	for _, mode := range []string{"direct", "group"} {
		for _, test := range []struct {
			name, body string
			count, dim int
			base64     bool
		}{
			{"text", `{"model":"unknown-model","input":"hello","dimensions":3}`, 1, 3, false},
			{"text batch", `{"model":"unknown-model","input":["hello","world"],"dimensions":4}`, 2, 4, false},
			{"tokens", `{"model":"unknown-model","input":[1,2,3],"dimensions":2}`, 1, 2, false},
			{"token batch", `{"model":"unknown-model","input":[[1,2],[3]],"dimensions":5,"encoding_format":"base64"}`, 2, 5, true},
			{"default dimensions", `{"model":"text-embedding-3-small","input":"hello"}`, 1, 1536, false},
			{"large dimensions", `{"model":"text-embedding-3-large","input":"hello"}`, 1, 3072, false},
			{"empty batch", `{"model":"unknown-model","input":[]}`, 0, 1536, false},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				// No limiter, repository, keys or HTTP client: any route selection,
				// mapping, concurrency check or upstream access would fail this test.
				service := &Service{}
				request := httptest.NewRequest(http.MethodPost, embeddingsPath, strings.NewReader(test.body))
				recorder := httptest.NewRecorder()
				if mode == "direct" {
					service.proxyDirect(recorder, request, Route{ModelMappingEnabled: true, ModelMapping: map[string]string{"unknown-model": "mapped-model"}})
				} else {
					service.proxySmartGroup(recorder, request, SmartGroup{})
				}
				if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
				}
				var response struct {
					Object string `json:"object"`
					Model  string `json:"model"`
					Data   []struct {
						Object    string          `json:"object"`
						Index     int             `json:"index"`
						Embedding json.RawMessage `json:"embedding"`
					} `json:"data"`
					Usage struct {
						PromptTokens int `json:"prompt_tokens"`
						TotalTokens  int `json:"total_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				var input struct{ Model string }
				_ = json.Unmarshal([]byte(test.body), &input)
				if response.Object != "list" || response.Model != input.Model || response.Data == nil || len(response.Data) != test.count || response.Usage.PromptTokens != 0 || response.Usage.TotalTokens != 0 {
					t.Fatalf("invalid local response: %s", recorder.Body.String())
				}
				for i, item := range response.Data {
					if item.Object != "embedding" || item.Index != i {
						t.Fatalf("invalid embedding index/object: %+v", item)
					}
					if test.base64 {
						var encoded string
						if err := json.Unmarshal(item.Embedding, &encoded); err != nil {
							t.Fatal(err)
						}
						decoded, err := base64.StdEncoding.DecodeString(encoded)
						if err != nil || len(decoded) != test.dim*4 {
							t.Fatalf("invalid base64 float32 vector: %v bytes=%d", err, len(decoded))
						}
						for _, value := range decoded {
							if value != 0 {
								t.Fatal("expected placeholder zero vector")
							}
						}
					} else {
						var vector []float32
						if err := json.Unmarshal(item.Embedding, &vector); err != nil || len(vector) != test.dim {
							t.Fatalf("invalid float vector: %v dimensions=%d", err, len(vector))
						}
						for _, value := range vector {
							if value != 0 {
								t.Fatal("expected placeholder zero vector")
							}
						}
					}
				}
			})
		}
	}
}

func TestEmbeddingsStillRequireAuthentication(t *testing.T) {
	response := httptest.NewRecorder()
	(&Service{}).PublicHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, embeddingsPath, strings.NewReader(`{"model":"model","input":"hello"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
}

func TestEmbeddingsCannotProbeOrAffectHealth(t *testing.T) {
	spec := routeProbeSpec{ModelID: "model", Path: embeddingsPath}
	if id := modelHealthID(Route{ID: "route"}, spec); id != "" {
		t.Fatalf("local embeddings have health ID %s", id)
	}
	if _, _, err := newRouteProbeRequest(context.Background(), spec); err == nil {
		t.Fatal("embeddings recovery request accepted")
	}
	if (&Service{}).performRouteProbe(context.Background(), Route{}, spec) || (&Service{}).executeRouteProbe(context.Background(), Route{}, spec).Success {
		t.Fatal("embeddings produced a recovery result")
	}
	if _, err := manualProbeSpec(RouteTestRequest{ModelID: "model", Protocol: "embeddings"}); err == nil {
		t.Fatal("embeddings accepted as a manual recovery test")
	}
	if paused, err := (&Limiter{}).ObserveRouteResult(context.Background(), nil, false, spec); err != nil || paused {
		t.Fatalf("local embeddings affected route health: %v %v", paused, err)
	}
	if paused, err := (&Limiter{}).ObserveModelResult(context.Background(), &Lease{modelHealthID: "legacy"}, false, false, Route{}, spec); err != nil || paused {
		t.Fatalf("local embeddings affected model health: %v %v", paused, err)
	}
}

func TestLegacyEmbeddingRecoveryRedisCleanup(t *testing.T) {
	for _, mode := range []string{"startup", "runtime", "claim"} {
		t.Run(mode, func(t *testing.T) {
			limiter, client := redisTestLimiter(t)
			ctx := context.Background()
			route := healthTestRoute(t, client)
			id := hashKey(route.ID + "legacy-embedding")
			goodID := hashKey(route.ID + "other-model")
			t.Cleanup(func() { _ = client.Del(ctx, modelHealthKey(id)).Err() })
			spec, _ := json.Marshal(routeProbeSpec{ModelID: "embedding-model", Path: embeddingsPath})
			due := float64(time.Now().Add(-time.Minute).UnixMilli())
			pipeline := client.TxPipeline()
			pipeline.HSet(ctx, healthKey(route.ID), "epoch", 7, "paused_until", time.Now().Add(30*time.Minute).UnixMilli(), "probe_spec", string(spec), "probe_token", "old", "failures", 3)
			pipeline.ZAdd(ctx, probeDueKey(), redis.Z{Member: route.ID, Score: due})
			pipeline.SAdd(ctx, routeModelIndexKey(route.ID), id, goodID)
			pipeline.HSet(ctx, modelHealthKey(id), "epoch", 5, "route_id", route.ID, "model_id", "embedding-model", "protocol", embeddingsPath, "isolated", 1, "probe_spec", string(spec), "probe_token", "old", "failures", 3)
			pipeline.HSet(ctx, modelHealthKey(goodID), "epoch", 2, "route_id", route.ID, "model_id", "other-model", "protocol", "/v1/responses", "isolated", 1, "failures", 3)
			pipeline.ZAdd(ctx, modelProbeDueKey(), redis.Z{Member: id, Score: due}, redis.Z{Member: goodID, Score: due})
			if _, err := pipeline.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "startup":
				if err := limiter.discardEmbeddingRecovery(ctx); err != nil {
					t.Fatal(err)
				}
			case "claim":
				// With no repository or HTTP client these must stop at cleanup.
				service := &Service{limiter: limiter}
				service.probePausedRoute(ctx, route.ID)
				service.probeIsolatedModel(ctx, id)
			}
			rows := []Route{route}
			rows[0].RemovedModels = map[string]RemovedRouteModel{"removed-embedding": {ModelID: "old", Protocol: embeddingsPath}}
			if err := limiter.PopulateRouteRuntime(ctx, rows); err != nil {
				t.Fatal(err)
			}
			if rows[0].PausedUntil != nil || rows[0].ConsecutiveFailures != 0 || len(rows[0].ModelHealth) != 1 || rows[0].ModelHealth[0].ModelID != "other-model" {
				t.Fatalf("embedding health still visible or other model changed: %+v", rows[0])
			}
			for _, queue := range []struct{ key, member string }{{probeDueKey(), route.ID}, {modelProbeDueKey(), id}} {
				if _, err := client.ZScore(ctx, queue.key, queue.member).Result(); err != redis.Nil {
					t.Fatalf("legacy embedding probe still queued: %v", err)
				}
			}
			if epoch, _ := client.HGet(ctx, modelHealthKey(id), "epoch").Int64(); epoch <= 5 {
				t.Fatal("cleanup did not fence in-flight embedding results")
			}
			if epoch, _ := client.HGet(ctx, healthKey(route.ID), "epoch").Int64(); epoch <= 7 {
				t.Fatal("cleanup did not fence in-flight route results")
			}
			if _, err := client.ZScore(ctx, modelProbeDueKey(), goodID).Result(); err != nil {
				t.Fatalf("other model probe was removed: %v", err)
			}
		})
	}
}
