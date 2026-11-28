package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRequestCapabilitiesUseAttachmentsAndReservedTools(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       requestCapabilities
	}{
		{"prompt mentions", `{"messages":[{"role":"user","content":"Read PDF report.pdf and run Python code_execution_20250522"}]}`, requestCapabilities{}},
		{"system mentions", `{"system":"application/pdf code_interpreter","input":"hello"}`, requestCapabilities{}},
		{"Claude PDF", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}]}]}`, requestCapabilities{PDF: true}},
		{"Claude PDF URL", `{"messages":[{"content":[{"type":"document","source":{"type":"url","url":"https://example.test/report.PDF?token=x"}}]}]}`, requestCapabilities{PDF: true}},
		{"Claude text document", `{"messages":[{"content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"PDF"}}]}]}`, requestCapabilities{}},
		{"Responses PDF", `{"input":[{"role":"user","content":[{"type":"input_file","filename":"scan.pdf","file_data":"data:application/pdf;base64,JVBERg=="}]}]}`, requestCapabilities{PDF: true}},
		{"Chat PDF", `{"messages":[{"content":[{"type":"file","file":{"filename":"scan.pdf","file_data":"data:application/pdf;base64,JVBERg=="}}]}]}`, requestCapabilities{PDF: true}},
		{"Gemini PDF", `{"contents":[{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"JVBERg=="}}]}]}`, requestCapabilities{PDF: true}},
		{"history PDF", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}]},{"role":"assistant","content":"OK"},{"role":"user","content":"Summarize"}]}`, requestCapabilities{PDF: true}},
		{"ordinary image", `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]}`, requestCapabilities{}},
		{"tool output PDF", `{"messages":[{"content":[{"type":"tool_result","content":[{"type":"document","source":{"media_type":"application/pdf"}}]}]}]}`, requestCapabilities{}},
		{"function output PDF", `{"input":[{"type":"function_call_output","output":{"type":"input_file","filename":"report.pdf"}}]}`, requestCapabilities{}},
		{"schema examples", `{"tools":[{"name":"parse","input_schema":{"examples":[{"type":"document","source":{"media_type":"application/pdf"}}]}}]}`, requestCapabilities{}},
		{"client Python function", `{"tools":[{"type":"function","function":{"name":"code_interpreter"}},{"name":"code_execution","input_schema":{"type":"object"}}]}`, requestCapabilities{}},
		{"Claude server code", `{"tools":[{"type":"code_execution_20250522","name":"code_execution"}]}`, requestCapabilities{CodeExecution: true}},
		{"Claude newer code", `{"tools":[{"type":"code_execution_20250825","name":"code_execution"}]}`, requestCapabilities{CodeExecution: true}},
		{"invalid reserved type", `{"tools":[{"type":"code_execution_notadate"}]}`, requestCapabilities{}},
		{"Responses interpreter", `{"tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`, requestCapabilities{CodeExecution: true}},
		{"Gemini execution", `{"tools":[{"codeExecution":{}}]}`, requestCapabilities{CodeExecution: true}},
		{"cutoff question is text", `{"messages":[{"role":"user","content":"What is your training knowledge cutoff date?"}]}`, requestCapabilities{}},
		{"signature mention is text", `{"messages":[{"role":"user","content":"Explain thinking signatures and web_search."}]}`, requestCapabilities{}},
		{"client search function", `{"tools":[{"name":"web_search","input_schema":{"type":"object"}}]}`, requestCapabilities{}},
		{"server search", `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, requestCapabilities{WebSearch: true}},
		{"dynamic filtering stays native", `{"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, requestCapabilities{WebSearch: true, NativeClaude: true, NativeState: true}},
		{"Messages schema", `{"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, requestCapabilities{JSONSchema: true}},
		{"Responses schema", `{"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, requestCapabilities{JSONSchema: true}},
		{"Chat schema", `{"response_format":{"type":"json_schema"}}`, requestCapabilities{JSONSchema: true}},
		{"adaptive thinking", `{"thinking":{"type":"adaptive"}}`, requestCapabilities{NativeClaude: true, Thinking: true}},
		{"disabled thinking", `{"thinking":{"type":"disabled"}}`, requestCapabilities{}},
		{"signed history", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"opaque"}]}]}`, requestCapabilities{NativeClaude: true, Thinking: true}},
		{"native search history", `{"messages":[{"role":"assistant","content":[{"type":"web_search_tool_result","content":[]}]}]}`, requestCapabilities{NativeClaude: true, NativeState: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.body), &payload); err != nil {
				t.Fatal(err)
			}
			if got := inspectRequestCapabilities(payload); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestDedicatedCapabilityMembersCoverWholeRequest(t *testing.T) {
	all := []string{"pdf", "code_execution", "json_schema", "web_search", "native_claude"}
	routes := []Route{
		{ID: "ordinary", RequestMatchMode: KeywordMatchModeSkipOnMatch, RequestMatchCapabilities: all},
		{ID: "mapped", RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: []string{"json_schema", "web_search"}, ModelMappingEnabled: true, ModelMapping: map[string]string{"alias": "gpt-6-luna"}},
		{ID: "native", RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: all},
	}
	for _, tc := range []struct {
		name  string
		facts requestCapabilities
		want  []string
	}{
		{"plain", requestCapabilities{}, []string{"ordinary"}},
		{"schema", requestCapabilities{JSONSchema: true}, []string{"mapped", "native"}},
		{"schema and search", requestCapabilities{JSONSchema: true, WebSearch: true}, []string{"mapped", "native"}},
		{"schema and PDF", requestCapabilities{JSONSchema: true, PDF: true}, []string{"native"}},
		{"search and thinking", requestCapabilities{WebSearch: true, NativeClaude: true}, []string{"native"}},
		{"execution", requestCapabilities{CodeExecution: true}, []string{"native"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := []string{}
			for _, route := range filterRoutesByRequest(tc.facts, routes, "alias") {
				got = append(got, route.ID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
	if got := filterRoutesByRequest(requestCapabilities{NativeClaude: true}, routes[1:2], "alias"); len(got) != 0 {
		t.Fatal("native request reached GPT")
	}
	unsafe := routes[1]
	unsafe.RequestMatchMode = KeywordMatchModeDisabled
	if got := filterRoutesByRequest(requestCapabilities{NativeClaude: true}, []Route{unsafe}, "alias"); len(got) != 0 {
		t.Fatal("disabled capability policy allowed native history into GPT")
	}
}

func TestRequestPolicyPairsOrdinaryAndMappedMembers(t *testing.T) {
	routes := []Route{
		{ID: "ordinary", RequestMatchMode: KeywordMatchModeSkipOnMatch, RequestMatchCapabilities: []string{"pdf", "code_execution"}},
		{ID: "mapped", RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: []string{"pdf", "code_execution"}, KeywordMatchMode: KeywordMatchModeRequireMatch},
	}
	for _, tc := range []struct {
		facts requestCapabilities
		want  string
	}{
		{requestCapabilities{}, "ordinary"}, {requestCapabilities{PDF: true}, "mapped"},
		{requestCapabilities{CodeExecution: true}, "mapped"}, {requestCapabilities{PDF: true, CodeExecution: true}, "mapped"},
	} {
		got := filterRoutesByRequest(tc.facts, routes)
		got = filterRoutesByKeywordsAndRequest("no keyword", []string{"special"}, got, tc.facts)
		if len(got) != 1 || got[0].ID != tc.want {
			t.Fatalf("facts=%+v routes=%+v", tc.facts, got)
		}
	}
	if got := filterRoutesByRequest(requestCapabilities{PDF: true}, routes[:1]); len(got) != 0 {
		t.Fatal("PDF fell back to ordinary member")
	}
	if got := filterRoutesByRequest(requestCapabilities{CodeExecution: true}, routes[:1]); len(got) != 0 {
		t.Fatal("execution fell back to ordinary member")
	}
	if got := filterRoutesByRequest(requestCapabilities{PDF: true}, []Route{{ID: "default"}}); len(got) != 1 {
		t.Fatal("disabled policy changed existing traffic")
	}
}

func TestRequestPolicyPersistsPartialUpdatesAndGroupIsolation(t *testing.T) {
	repo := memberPauseTestRepository(t)
	seedPauseMember(t, repo, Route{ID: "capability-route"}, "a", "b")
	svc := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	mode, caps := KeywordMatchModeRequireMatch, []string{"pdf", "code_execution", "json_schema", "web_search", "native_claude", "pdf"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "capability-route", UpdateMemberPolicyRequest{RequestMatchMode: &mode, RequestMatchCapabilities: &caps}); err != nil {
		t.Fatal(err)
	}
	priority := 7
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "capability-route", UpdateMemberPolicyRequest{Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"a", "b"} {
		for _, model := range []string{"", "model"} {
			members, err := NewRepository(repo.db).ListGroupRoutes(ctx, group, true, model)
			if err != nil || len(members) != 1 {
				t.Fatalf("members=%+v err=%v", members, err)
			}
			member := members[0]
			if group == "a" {
				if member.RequestMatchMode != mode || !reflect.DeepEqual(member.RequestMatchCapabilities, []string{"pdf", "code_execution", "json_schema", "web_search", "native_claude"}) || member.Priority != 7 {
					t.Fatalf("policy not persisted: %+v", member)
				}
			} else if member.RequestMatchMode != KeywordMatchModeDisabled || len(member.RequestMatchCapabilities) != 0 {
				t.Fatal("policy leaked to another group")
			}
		}
	}
	empty := []string{}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "capability-route", UpdateMemberPolicyRequest{RequestMatchCapabilities: &empty}); err == nil {
		t.Fatal("cleared capabilities under an active policy")
	}
	if err := svc.UpdateMemberPolicy(ctx, "other-user", "a", "capability-route", UpdateMemberPolicyRequest{RequestMatchMode: &mode}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unauthorized policy update: %v", err)
	}
	mode = KeywordMatchModeDisabled
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "capability-route", UpdateMemberPolicyRequest{RequestMatchMode: &mode, RequestMatchCapabilities: &empty}); err != nil {
		t.Fatal(err)
	}
	mode = KeywordMatchModeSkipOnMatch
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "capability-route", UpdateMemberPolicyRequest{RequestMatchMode: &mode}); err == nil {
		t.Fatal("enabled policy without capabilities")
	}
}

func TestRequestPolicyRejectsUnknownValues(t *testing.T) {
	svc := &Service{accounts: proxyTestAccount{}}
	mode, caps := "sometimes", []string{"pdf", "shell"}
	for _, input := range []UpdateMemberPolicyRequest{{RequestMatchMode: &mode}, {RequestMatchCapabilities: &caps}} {
		var invalid *requestError
		err := svc.UpdateMemberPolicy(context.Background(), "user", "a", "route", input)
		if !errors.As(err, &invalid) || invalid.Status != 400 {
			t.Fatalf("invalid policy err=%v", err)
		}
	}
}

func TestCapabilitySmartGroupDispatchAndFailureDoNotFallBackToOrdinary(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var ordinaryCalls, mappedCalls atomic.Int64
	var failMapped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			ordinaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"normal","type":"message","model":"model","role":"assistant","content":[{"type":"text","text":"ordinary"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		mappedCalls.Add(1)
		if failMapped.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+offloadTextResponse+"}\n\n")
	}))
	defer server.Close()
	svc, ordinary := preparedProbeService(t, server)
	svc.repository, svc.limiter, svc.accounts = repo, limiter, proxyTestAccount{}
	ordinary.ID = healthTestRoute(t, client).ID
	mapped := ordinary
	mapped.ID = healthTestRoute(t, client).ID
	cleanupRedisTestRoutes(t, client, []Route{ordinary, mapped})
	seedPauseMember(t, repo, ordinary, "capability-group")
	seedPauseMember(t, repo, mapped)
	ctx := context.Background()
	if err := repo.AddMember(ctx, "user", "workspace", "capability-group", mapped.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceRouteModels(ctx, mapped.ID, []Model{{ID: "gpt-5.4", Object: "model"}}); err != nil {
		t.Fatal(err)
	}
	skip, required, caps := KeywordMatchModeSkipOnMatch, KeywordMatchModeRequireMatch, []string{"pdf", "code_execution"}
	enabled := true
	mapping := map[string]string{"model": "gpt-5.4"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "capability-group", ordinary.ID, UpdateMemberPolicyRequest{RequestMatchMode: &skip, RequestMatchCapabilities: &caps}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "capability-group", mapped.ID, UpdateMemberPolicyRequest{RequestMatchMode: &required, RequestMatchCapabilities: &caps, ModelMappingEnabled: &enabled, ModelMapping: &mapping, KeywordMatchMode: &required}); err != nil {
		t.Fatal(err)
	}
	group, err := repo.GetSmartGroup(ctx, "capability-group")
	if err != nil {
		t.Fatal(err)
	}
	group.Keywords = []string{"not-present"}
	request := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)), *group)
		return w
	}
	plain := `{"model":"model","max_tokens":300,"messages":[{"role":"user","content":"Explain PDF and Python."}]}`
	if w := request(plain); w.Code != 200 || ordinaryCalls.Load() != 1 || mappedCalls.Load() != 0 {
		t.Fatalf("plain status=%d body=%s", w.Code, w.Body.String())
	}
	pdf := strings.Replace(offloadPDFRequest, `"model":"alias"`, `"model":"model"`, 1)
	if w := request(pdf); w.Code != 200 || ordinaryCalls.Load() != 1 || mappedCalls.Load() != 1 || !strings.Contains(w.Body.String(), `"model":"model"`) {
		t.Fatalf("PDF status=%d ordinary=%d mapped=%d body=%s", w.Code, ordinaryCalls.Load(), mappedCalls.Load(), w.Body.String())
	}
	failMapped.Store(true)
	if w := request(pdf); w.Code != http.StatusBadGateway || ordinaryCalls.Load() != 1 || mappedCalls.Load() < 2 {
		t.Fatalf("failed PDF fell back: status=%d ordinary=%d mapped=%d body=%s", w.Code, ordinaryCalls.Load(), mappedCalls.Load(), w.Body.String())
	}
}
