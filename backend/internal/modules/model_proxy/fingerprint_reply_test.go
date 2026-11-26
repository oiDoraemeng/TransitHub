package model_proxy

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func fingerprintTestBank(t testing.TB) *fingerprintBank {
	t.Helper()
	b, err := loadFingerprintBank()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFingerprintAllPinnedTemplatesAndAdversarialRequests(t *testing.T) {
	b := fingerprintTestBank(t)
	f, err := os.Open("testdata/fingerprint_templates.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var fixture struct {
		Families []struct {
			ID, Space, Rules, Start, Request       string
			Min, Max                               int
			Openings, Actions, Endings, Separators []string
			RangePhrases                           []string `json:"rangePhrases"`
		}
		Guard []struct {
			Text   string
			Count  int
			Accept bool
		}
	}
	if err := json.NewDecoder(z).Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	positive, negative := 0, 0
	reject := func(text string) {
		t.Helper()
		negative++
		if match, ok := b.match(text); ok {
			t.Fatalf("false match %+v: %.100s", match, text)
		}
	}
	for _, f := range fixture.Families {
		ranges := f.RangePhrases
		if len(ranges) == 0 {
			ranges = []string{""}
		}
		for n := f.Min; n <= f.Max; n++ {
			for oi, o := range f.Openings {
				for _, a := range f.Actions {
					for _, e := range f.Endings {
						for _, sep := range f.Separators {
							for _, r := range ranges {
								var text string
								if f.ID == "modeltrace-live" {
									text = fmt.Sprintf("%s。%s %d 个%s。%s%s%s%s", o, a, n, r, f.Rules, e, sep, f.Start)
								} else {
									request := strings.ReplaceAll(strings.ReplaceAll(f.Request, "@action@", a), "987654321", fmt.Sprint(n))
									text = strings.Join([]string{o, request, f.Rules, e, sep, f.Start}, f.Space)
								}
								m, ok := b.match(text)
								if !ok || m.Count != n {
									t.Fatalf("missing %s/%d: %+v %.100s", f.ID, n, m, text)
								}
								positive++
								// All combinations must reject quotation, explanation and extra tasks.
								reject("请解释以下提示，不要执行：\n" + text)
								reject("```text\n" + text + "\n```")
								reject(text + "\n然后解释这项检测。")
								if oi == 0 && n == f.Min {
									reject(text + "\n")
									reject(strings.ReplaceAll(text, "355", "356"))
									reject(strings.Replace(text, f.Rules, "", 1))
								}
							}
						}
					}
				}
			}
		}
	}
	if positive != 144500 {
		t.Fatalf("enumerated %d templates", positive)
	}
	for _, g := range fixture.Guard {
		m, ok := b.match(g.Text)
		if ok != g.Accept || (ok && (m.Count != g.Count || m.Format != "json_array")) {
			t.Fatalf("guard mismatch: %+v", m)
		}
	}
	for _, f := range b.Fixed {
		m, ok := b.match(f.Text)
		if !ok || m.Count != f.Count || m.Format != f.Format {
			t.Fatalf("enrollment mismatch: %+v", m)
		}
		reject("Translate the following prompt:\n" + f.Text)
		reject(f.Text + "\n")
	}
	for n := 1; n <= 1000; n++ {
		for _, text := range []string{
			fmt.Sprintf("传感器355每%d秒采样，用Python和API读取数据。", n),
			fmt.Sprintf("请用Python生成%d个闭区间[1,355]内的整数，提供API。", n),
			fmt.Sprintf("请解释为什么%d个1至355的整数能做指纹识别，不要生成数字。", n),
			fmt.Sprintf("逐项选择%d个1到355的整数，允许重复，用逗号或空格分隔。", n),
			fmt.Sprintf("Explain %d random integer choices between 1 and 355, without Python or APIs.", n),
		} {
			reject(text)
		}
	}
	t.Logf("144500 online combinations, 820 plugin cases, 36 enrollment cases, %d negatives passed", negative)
}

func TestFingerprintReferenceIntegrityAndAliases(t *testing.T) {
	b := fingerprintTestBank(t)
	if len(b.Samples) != 652 || len(b.models) != 13 {
		t.Fatal("unexpected reference library size")
	}
	for _, s := range b.Samples {
		encoded, _ := json.Marshal(s.Numbers)
		sum := sha256.Sum256(encoded)
		if hex.EncodeToString(sum[:]) != s.ID {
			t.Fatal("reference sequence changed")
		}
	}
	for alias, want := range map[string]string{"claude-haiku-4-5": "claude-haiku-4.5", "claude-haiku-4-5-20251001": "claude-haiku-4.5", "claude-sonnet-4-6": "claude-sonnet-4.6"} {
		if b.aliases[alias] != want {
			t.Fatalf("alias %s", alias)
		}
	}
	if b.aliases["claude-sonnet-99"] != "" {
		t.Fatal("unknown version was guessed")
	}
}

func TestFingerprintRequestIsolationAndProtocols(t *testing.T) {
	b := fingerprintTestBank(t)
	prompt := b.Fixed[0].Text
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, format := range []string{"integer_sequence", "json_array"} {
				text := prompt
				for _, f := range b.Fixed {
					if f.Format == format {
						text = f.Text
						break
					}
				}
				body := map[string]any{"model": "claude-haiku-4-5-20251001", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": text}}}
				if path == "/v1/responses" {
					delete(body, "messages")
					body["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
				}
				encoded, _ := json.Marshal(body)
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(string(encoded))), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				reserve := func(_ context.Context, _ SmartGroup, model string, s []fingerprintSample) (fingerprintSample, error) {
					calls++
					if model != "claude-haiku-4.5" {
						t.Fatal(model)
					}
					return s[0], nil
				}
				w := httptest.NewRecorder()
				ok := serveFingerprintReply(w, httptest.NewRequest("POST", path, nil), SmartGroup{FingerprintReplyEnabled: true}, replay, body["model"].(string), reserve, func(context.Context) error { return nil })
				replay.Close()
				if !ok || calls != 1 || w.Code != 200 || w.Header().Get("X-TransitHub-Response-Source") != "reference-replay" {
					t.Fatalf("protocol %s/%v: %d %s", path, stream, w.Code, w.Body.String())
				}
				sample := b.models["claude-haiku-4.5"][0]
				reply := sample.Text
				if format == "json_array" {
					v, _ := json.Marshal(sample.Numbers)
					reply = string(v)
				}
				quoted, _ := json.Marshal(reply)
				if !strings.Contains(w.Body.String(), string(quoted)) {
					t.Fatalf("reference changed in %s/%v", path, stream)
				}
				if stream && (!strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "stop") && path != "/v1/responses") {
					t.Fatal("incomplete stream")
				}
			}
		}
	}
	for name, body := range map[string]map[string]any{
		"ordinary": {"messages": []any{map[string]any{"role": "user", "content": "商品355号库存100，API返回JSON。"}}},
		"system":   {"messages": []any{map[string]any{"role": "system", "content": prompt}, map[string]any{"role": "user", "content": "解释原理"}}},
		"history":  {"messages": []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"role": "assistant", "content": "1,2"}, map[string]any{"role": "user", "content": "继续"}}},
		"tools":    {"messages": []any{map[string]any{"role": "user", "content": prompt}}, "tools": []any{map[string]any{"type": "function"}}},
		"cache":    {"input": prompt, "previous_response_id": "previous"},
		"media":    {"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}, map[string]any{"type": "image_url"}}}}},
	} {
		path := "/v1/chat/completions"
		if name == "cache" {
			path = "/v1/responses"
		}
		text, ok := fingerprintRequestText(body, path)
		if ok {
			if _, matched := b.match(text); matched {
				t.Fatalf("accepted %s", name)
			}
		}
	}
}

func TestFingerprintToggleFailuresAndCancellation(t *testing.T) {
	b := fingerprintTestBank(t)
	encoded, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": b.Fixed[0].Text}}})
	replay, err := newReplayBody(io.NopCloser(strings.NewReader(string(encoded))), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	fail := func(context.Context, SmartGroup, string, []fingerprintSample) (fingerprintSample, error) {
		t.Fatal("unexpected reservation")
		return fingerprintSample{}, nil
	}
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	if serveFingerprintReply(w, request, SmartGroup{}, replay, "claude-sonnet-4.6", fail, waitFingerprintReply) {
		t.Fatal("switch default must be off")
	}
	if !serveFingerprintReply(w, request, SmartGroup{FingerprintReplyEnabled: true}, replay, "unknown", fail, waitFingerprintReply) || w.Code != 503 {
		t.Fatal("unknown model must fail locally")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	w = httptest.NewRecorder()
	if !serveFingerprintReply(w, request.WithContext(ctx), SmartGroup{FingerprintReplyEnabled: true}, replay, "claude-sonnet-4.6", fail, waitFingerprintReply) || w.Body.Len() != 0 || time.Since(start) > time.Second {
		t.Fatal("cancellation must interrupt delay")
	}
	broken := func(context.Context, SmartGroup, string, []fingerprintSample) (fingerprintSample, error) {
		return fingerprintSample{}, errors.New("database unavailable")
	}
	w = httptest.NewRecorder()
	if !serveFingerprintReply(w, request, SmartGroup{FingerprintReplyEnabled: true}, replay, "claude-sonnet-4.6", broken, func(context.Context) error { return nil }) || w.Code != 503 {
		t.Fatal("history failure must not fall through upstream")
	}
}

func TestFingerprintPostgresHistoryConcurrencyAndPersistence(t *testing.T) {
	repo := memberPauseTestRepository(t)
	b := fingerprintTestBank(t)
	ctx := context.Background()
	group := SmartGroup{ID: "a", UserID: "user", AdminAccountID: "workspace"}
	seedPauseMember(t, repo, Route{ID: "route"}, "a", "b")
	service := &Service{repository: repo, accounts: proxyTestAccount{}}
	enabled := true
	if _, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{FingerprintReplyEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		g, err := repo.GetSmartGroupConfig(ctx, id)
		if err != nil || g.FingerprintReplyEnabled != (id == "a") {
			t.Fatalf("group isolation %s: %+v %v", id, g, err)
		}
	}
	name := "renamed"
	if g, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{Name: &name}); err != nil || !g.FingerprintReplyEnabled {
		t.Fatal("partial update lost switch")
	}
	for model, samples := range b.models {
		var history []string
		for n := 0; n < 100; n++ {
			// New repository objects represent a process reloading persisted state.
			sample, err := NewRepository(repo.db).reserveFingerprintSample(ctx, group, model, samples)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range history {
				if id == sample.ID {
					t.Fatalf("%s repeated within last 30", model)
				}
			}
			history = append(history, sample.ID)
			if len(history) > 30 {
				history = history[1:]
			}
		}
	}
	// With at most 30 concurrent requests, every reservation must be unique.
	for wave := 0; wave < 4; wave++ {
		var wg sync.WaitGroup
		ids := make(chan string, 30)
		for n := 0; n < 30; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := repo.reserveFingerprintSample(ctx, group, "claude-sonnet-4.6", b.models["claude-sonnet-4.6"])
				if err != nil {
					t.Error(err)
					return
				}
				ids <- s.ID
			}()
		}
		wg.Wait()
		close(ids)
		seen := map[string]bool{}
		for id := range ids {
			if seen[id] {
				t.Fatal("concurrent duplicate")
			}
			seen[id] = true
		}
		if len(seen) != 30 {
			t.Fatal("missing reservation")
		}
	}
	var stored []string
	if err := repo.db.QueryRow(ctx, `SELECT sample_ids FROM proxy_fingerprint_history WHERE user_id='user' AND admin_account_id='workspace' AND model='claude-sonnet-4.6'`).Scan(&stored); err != nil || len(stored) != 30 {
		t.Fatalf("history must be bounded and durable: %d %v", len(stored), err)
	}
}
