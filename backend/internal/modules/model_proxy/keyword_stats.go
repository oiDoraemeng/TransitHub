package model_proxy

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var incrementKeywordMatchesScript = redis.NewScript(`
for index = 2, #ARGV do
  redis.call('HINCRBY', KEYS[1], ARGV[index], 1)
end
redis.call('EXPIREAT', KEYS[1], ARGV[1])
return #ARGV - 1
`)

func keywordMatchCountKey(groupID string, now time.Time) (string, int64) {
	_, expiresAt := routeRequestCountKey(now)
	return "proxy:keyword-count:{" + groupID + "}:" + now.In(shanghaiLocation).Format("20060102"), expiresAt
}

// Count once at request entry, before local replies or route retries. A request
// may match several keywords, but repeating one keyword does not add hits.
func (l *Limiter) RecordKeywordMatches(ctx context.Context, group SmartGroup, input string) error {
	if l == nil || l.redis == nil || group.ID == "" || len(group.Keywords) == 0 {
		return nil
	}
	key, expiresAt := keywordMatchCountKey(group.ID, time.Now())
	args := []any{expiresAt}
	for _, keyword := range matchedGroupKeywords(group.Keywords, input) {
		args = append(args, keyword)
	}
	if len(args) == 1 {
		return nil
	}
	return incrementKeywordMatchesScript.Run(ctx, l.redis, []string{key}, args...).Err()
}

func matchedGroupKeywords(keywords []string, input string) []string {
	input = strings.ToLower(input)
	var matched []string
	seen := make(map[string]bool)
	for _, keyword := range keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword != "" && !seen[keyword] && strings.Contains(input, keyword) {
			matched = append(matched, keyword)
			seen[keyword] = true
		}
	}
	return matched
}

type dailyCountBatch struct {
	expiresAt int64
	counts    map[string]int64
}

// Aggregate by group/keyword/route, not by request: memory and Redis writes
// grow with configured entries instead of concurrent traffic. Request handling
// only takes a short mutex; no goroutine or network operation per statistic.
type requestStatistics struct {
	mu      sync.Mutex
	pending map[string]*dailyCountBatch
	limiter *Limiter
}

func newRequestStatistics(limiter *Limiter) *requestStatistics {
	return &requestStatistics{limiter: limiter, pending: make(map[string]*dailyCountBatch)}
}

func (s *requestStatistics) add(key string, expiresAt int64, fields []string) {
	if len(fields) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.pending[key]
	if batch == nil {
		batch = &dailyCountBatch{expiresAt: expiresAt, counts: make(map[string]int64)}
		s.pending[key] = batch
	}
	for _, field := range fields {
		batch.counts[field]++
	}
}

const flushDailyCountsScript = `
for index = 2, #ARGV, 2 do
  redis.call('HINCRBY', KEYS[1], ARGV[index], ARGV[index + 1])
end
redis.call('EXPIREAT', KEYS[1], ARGV[1])
return 1
`

func (s *requestStatistics) flush(ctx context.Context) {
	if s.limiter == nil || s.limiter.redis == nil {
		return
	}
	s.mu.Lock()
	pending := s.pending
	s.pending = make(map[string]*dailyCountBatch)
	s.mu.Unlock()
	pipe := s.limiter.redis.Pipeline()
	commands := make(map[string]*redis.Cmd)
	for key, batch := range pending {
		if batch.expiresAt <= time.Now().Unix() {
			continue
		}
		args := []any{batch.expiresAt}
		for field, count := range batch.counts {
			args = append(args, field, count)
		}
		// EVAL avoids a separate NOSCRIPT round trip after a Redis restart.
		commands[key] = pipe.Eval(ctx, flushDailyCountsScript, []string{key}, args...)
	}
	if len(commands) == 0 {
		return
	}
	_, _ = pipe.Exec(ctx)
	for key, command := range commands {
		if command.Err() == nil {
			continue
		}
		s.mu.Lock()
		batch := s.pending[key]
		if batch == nil {
			s.pending[key] = pending[key]
		} else {
			for field, count := range pending[key].counts {
				batch.counts[field] += count
			}
		}
		s.mu.Unlock()
	}
}

func (s *Service) requestStatisticsLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	flush := func() {
		writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.statistics.flush(writeCtx)
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Service) recordKeywordMatches(ctx context.Context, group SmartGroup, input string) {
	if s.statistics != nil {
		key, expiresAt := keywordMatchCountKey(group.ID, time.Now())
		s.statistics.add(key, expiresAt, matchedGroupKeywords(group.Keywords, input))
		return
	}
	_ = s.limiter.RecordKeywordMatches(ctx, group, input)
}

func (l *Limiter) PopulateKeywordMatchCounts(ctx context.Context, groups []SmartGroup) error {
	var pipe redis.Pipeliner
	if l != nil && l.redis != nil {
		pipe = l.redis.Pipeline()
	}
	commands := make(map[int]*redis.SliceCmd)
	now := time.Now()
	for i := range groups {
		groups[i].KeywordMatchCounts = make(map[string]int64, len(groups[i].Keywords))
		fields := make([]string, 0, len(groups[i].Keywords))
		for _, keyword := range groups[i].Keywords {
			groups[i].KeywordMatchCounts[keyword] = 0
			fields = append(fields, strings.ToLower(strings.TrimSpace(keyword)))
		}
		if pipe != nil && len(fields) > 0 {
			key, _ := keywordMatchCountKey(groups[i].ID, now)
			commands[i] = pipe.HMGet(ctx, key, fields...)
		}
	}
	if len(commands) == 0 {
		return nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	for i, command := range commands {
		for j, value := range command.Val() {
			if text, ok := value.(string); ok {
				count, _ := strconv.ParseInt(text, 10, 64)
				groups[i].KeywordMatchCounts[groups[i].Keywords[j]] = count
			}
		}
	}
	return nil
}
