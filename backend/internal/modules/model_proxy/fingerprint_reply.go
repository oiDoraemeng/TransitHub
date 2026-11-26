package model_proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const fingerprintHistorySize = 30 // Ten ordinary rounds of three challenges.
const fingerprintDelayMin = 3 * time.Second
const fingerprintDelayMax = 9 * time.Second

//go:embed fingerprints/reference.json.gz
var fingerprintReferenceData []byte

type fingerprintCheck struct {
	Regex    string   `json:"regex"`
	Required string   `json:"required_phrase"`
	Any      []string `json:"any_phrase"`
	compiled *regexp.Regexp
}

type fingerprintProfile struct {
	ID        string             `json:"id"`
	CountMin  int                `json:"count_min"`
	CountMax  int                `json:"count_max"`
	Format    string             `json:"reply_format"`
	AutoReply bool               `json:"auto_reply"`
	Groups    []fingerprintCheck `json:"groups"`
	Pattern   string             `json:"whole_task_regex"`
	compiled  *regexp.Regexp
}

type fingerprintMatch struct {
	Profile string
	Count   int
	Format  string
}

type fingerprintFixed struct {
	Text   string `json:"text"`
	Count  int    `json:"count"`
	Format string `json:"format"`
}

type fingerprintSample struct {
	Model   string `json:"model"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Numbers []int  `json:"numbers"`
}

type fingerprintBank struct {
	Profiles []fingerprintProfile `json:"profiles"`
	Fixed    []fingerprintFixed   `json:"fixed"`
	Samples  []fingerprintSample  `json:"samples"`
	fixed    map[string]fingerprintMatch
	models   map[string][]fingerprintSample
	aliases  map[string]string
}

var loadFingerprintBank = sync.OnceValues(func() (*fingerprintBank, error) {
	r, err := gzip.NewReader(bytes.NewReader(fingerprintReferenceData))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var bank fingerprintBank
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&bank); err != nil {
		return nil, err
	}
	bank.fixed = make(map[string]fingerprintMatch)
	bank.models = make(map[string][]fingerprintSample)
	bank.aliases = make(map[string]string)
	for i := range bank.Profiles {
		p := &bank.Profiles[i]
		if !p.AutoReply {
			continue
		}
		if p.compiled, err = regexp.Compile(p.Pattern); err != nil {
			return nil, err
		}
		for j := range p.Groups {
			g := &p.Groups[j]
			if g.Regex != "" {
				if g.compiled, err = regexp.Compile(g.Regex); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, f := range bank.Fixed {
		bank.fixed[f.Text] = fingerprintMatch{Profile: "modeltrace-enrollment", Count: f.Count, Format: f.Format}
	}
	seen := make(map[string]bool)
	for _, s := range bank.Samples {
		if len(s.ID) != 64 || s.Text == "" || len(s.Numbers) < 189 || seen[s.Model+":"+s.ID] {
			return nil, errors.New("invalid reference sample")
		}
		for _, n := range s.Numbers {
			if n < 1 || n > 355 {
				return nil, errors.New("invalid reference integer")
			}
		}
		seen[s.Model+":"+s.ID] = true
		bank.models[s.Model] = append(bank.models[s.Model], s)
		bank.aliases[s.Model] = s.Model
		bank.aliases[strings.ReplaceAll(s.Model, ".", "-")] = s.Model
	}
	for _, samples := range bank.models {
		if len(samples) <= fingerprintHistorySize {
			return nil, errors.New("insufficient reference samples")
		}
	}
	bank.aliases["claude-haiku-4-5-20251001"] = "claude-haiku-4.5"
	return &bank, nil
})

func (b *fingerprintBank) match(text string) (fingerprintMatch, bool) {
	if len(text) > 128<<10 {
		return fingerprintMatch{}, false
	}
	if match, ok := b.fixed[text]; ok {
		return match, true
	}
	for _, p := range b.Profiles {
		if !p.AutoReply {
			continue
		} // Tool submissions must retain their original flow.
		matches := true
		for _, g := range p.Groups {
			ok := false
			switch {
			case g.compiled != nil:
				ok = g.compiled.MatchString(text)
			case g.Required != "":
				ok = strings.Contains(text, g.Required)
			default:
				for _, phrase := range g.Any {
					if strings.Contains(text, phrase) {
						ok = true
						break
					}
				}
			}
			if !ok {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		m := p.compiled.FindStringSubmatch(text)
		if len(m) < 2 || m[0] != text {
			continue
		}
		count, err := strconv.Atoi(m[1])
		if err == nil && count >= p.CountMin && count <= p.CountMax {
			return fingerprintMatch{Profile: p.ID, Count: count, Format: p.Format}, true
		}
	}
	return fingerprintMatch{}, false
}

// Only a complete, standalone user task is eligible. System instructions are
// never a keyword source, and quoted tasks, history, tools and media are excluded.
func fingerprintRequestText(input map[string]any, path string) (string, bool) {
	if hasLocalReplyContextHint(input) {
		return "", false
	}
	for _, k := range []string{"tools", "functions"} {
		if value := input[k]; value != nil {
			items, ok := value.([]any)
			if !ok || len(items) != 0 {
				return "", false
			}
		}
	}
	var messages []any
	switch path {
	case "/v1/chat/completions", "/v1/messages":
		messages, _ = input["messages"].([]any)
	case "/v1/responses":
		if text, ok := input["input"].(string); ok {
			return text, true
		}
		messages, _ = input["input"].([]any)
	default:
		return "", false
	}
	text, users := "", 0
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok || hasLocalReplyContextHint(m) {
			return "", false
		}
		for _, key := range []string{"tool_calls", "function_call", "tool_call_id"} {
			if m[key] != nil {
				return "", false
			}
		}
		role, _ := m["role"].(string)
		if role != "user" && role != "system" && role != "developer" {
			return "", false
		}
		if role != "user" {
			continue
		}
		users++
		if users > 1 {
			return "", false
		}
		if content, ok := m["content"].(string); ok {
			text = content
			continue
		}
		content, ok := m["content"].([]any)
		if !ok || len(content) != 1 {
			return "", false
		}
		block, ok := content[0].(map[string]any)
		if !ok || hasLocalReplyContextHint(block) {
			return "", false
		}
		kind, _ := block["type"].(string)
		if kind != "text" && kind != "input_text" {
			return "", false
		}
		text, ok = block["text"].(string)
		if !ok {
			return "", false
		}
	}
	return text, users == 1
}

func fingerprintRandomIndex(count int) (int, error) {
	if count <= 0 {
		return 0, errors.New("no reference samples available")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

func waitFingerprintReply(ctx context.Context) error {
	n, err := fingerprintRandomIndex(int((fingerprintDelayMax-fingerprintDelayMin)/time.Millisecond) + 1)
	if err != nil {
		return err
	}
	timer := time.NewTimer(fingerprintDelayMin + time.Duration(n)*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type fingerprintReserve func(context.Context, SmartGroup, string, []fingerprintSample) (fingerprintSample, error)

func (s *Service) serveGroupFingerprintReply(w http.ResponseWriter, r *http.Request, group SmartGroup, replay *replayBody, model string) bool {
	return serveFingerprintReply(w, r, group, replay, model, s.repository.reserveFingerprintSample, waitFingerprintReply)
}

func serveFingerprintReply(w http.ResponseWriter, r *http.Request, group SmartGroup, replay *replayBody, model string, reserve fingerprintReserve, wait func(context.Context) error) bool {
	if !group.FingerprintReplyEnabled || r.Method != http.MethodPost {
		return false
	}
	input, err := replay.Object()
	if err != nil {
		return false
	}
	text, ok := fingerprintRequestText(input, r.URL.Path)
	if !ok {
		return false
	}
	bank, err := loadFingerprintBank()
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "reference_unavailable", "reference library is unavailable")
		return true
	}
	match, ok := bank.match(text)
	if !ok {
		return false
	}
	canonical := bank.aliases[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/")]
	samples := bank.models[canonical]
	if len(samples) <= fingerprintHistorySize {
		writeOpenAIError(w, http.StatusServiceUnavailable, "reference_unavailable", "no reference library for the requested model")
		return true
	}
	if err := wait(r.Context()); err != nil {
		if r.Context().Err() == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "reference_unavailable", "reference reply could not be scheduled")
		}
		return true
	}
	// Reserve after the delay so completion order closely follows history order.
	sample, err := reserve(r.Context(), group, canonical, samples)
	if err != nil {
		if r.Context().Err() == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "reference_unavailable", "reference history is unavailable")
		}
		return true
	}
	reply := sample.Text
	if match.Format == "json_array" {
		encoded, _ := json.Marshal(sample.Numbers)
		reply = string(encoded)
	}
	id, err := randomID("")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "failed to create response ID")
		return true
	}
	w.Header().Set("X-TransitHub-Response-Source", "reference-replay")
	w.Header().Set("X-TransitHub-Reference-Model", canonical)
	w.Header().Set("X-TransitHub-Reference-Sample", sample.ID)
	w.Header().Set("Cache-Control", "no-store")
	streaming, _ := input["stream"].(bool)
	switch r.URL.Path {
	case "/v1/chat/completions":
		writeLocalChatReply(w, input, model, id, time.Now().Unix(), streaming, reply)
	case "/v1/responses":
		writeLocalResponsesReply(w, model, id, time.Now().Unix(), streaming, reply)
	case "/v1/messages":
		writeLocalMessagesReply(w, model, id, streaming, reply)
	}
	return true
}

func (r *Repository) reserveFingerprintSample(ctx context.Context, group SmartGroup, model string, samples []fingerprintSample) (fingerprintSample, error) {
	if r == nil || r.db == nil {
		return fingerprintSample{}, errors.New("reference history is unavailable")
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fingerprintSample{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO proxy_fingerprint_history (user_id,admin_account_id,model) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, group.UserID, group.AdminAccountID, model); err != nil {
		return fingerprintSample{}, err
	}
	var history []string
	if err = tx.QueryRow(ctx, `SELECT sample_ids FROM proxy_fingerprint_history WHERE user_id=$1 AND admin_account_id=$2 AND model=$3 FOR UPDATE`, group.UserID, group.AdminAccountID, model).Scan(&history); err != nil {
		return fingerprintSample{}, err
	}
	used := make(map[string]bool, len(history))
	for _, id := range history {
		used[id] = true
	}
	available := make([]int, 0, len(samples))
	for i, s := range samples {
		if !used[s.ID] {
			available = append(available, i)
		}
	}
	n, err := fingerprintRandomIndex(len(available))
	if err != nil {
		return fingerprintSample{}, fmt.Errorf("reserve reference: %w", err)
	}
	chosen := samples[available[n]]
	history = append(history, chosen.ID)
	if len(history) > fingerprintHistorySize {
		history = history[len(history)-fingerprintHistorySize:]
	}
	if _, err = tx.Exec(ctx, `UPDATE proxy_fingerprint_history SET sample_ids=$4,updated_at=now() WHERE user_id=$1 AND admin_account_id=$2 AND model=$3`, group.UserID, group.AdminAccountID, model, history); err != nil {
		return fingerprintSample{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return fingerprintSample{}, err
	}
	return chosen, nil
}
