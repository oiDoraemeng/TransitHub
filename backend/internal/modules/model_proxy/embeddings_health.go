package model_proxy

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Preserve epochs when removing obsolete circuits so in-flight results from an
// older instance cannot recreate an Embeddings pause after an upgrade.
const embeddingModelHealthLua = `
local function discardEmbeddingModel(key, dueKey, indexKey, id)
  local embedding = redis.call('HGET', key, 'protocol') == '/v1/embeddings'
  if not embedding then
    local encoded = redis.call('HGET', key, 'probe_spec')
    if encoded then
      local ok, spec = pcall(cjson.decode, encoded)
      embedding = ok and type(spec) == 'table' and spec.path == '/v1/embeddings'
    end
  end
  if not embedding then return false end
  redis.call('HINCRBY', key, 'epoch', 1)
  redis.call('HSET', key, 'failures', 0, 'probe_attempts', 0)
  redis.call('HDEL', key, 'isolated', 'isolated_at', 'probe_spec', 'probe_token', 'probe_disabled')
  redis.call('ZREM', dueKey, id)
  if indexKey then redis.call('SREM', indexKey, id) end
  return true
end
`

var discardEmbeddingModelScript = redis.NewScript(embeddingModelHealthLua + `
return discardEmbeddingModel(KEYS[1], KEYS[2], KEYS[3], ARGV[1]) and 1 or 0
`)

var discardEmbeddingRouteScript = redis.NewScript(routeHealthLua + `
return discardEmbeddingRoute(KEYS[1], KEYS[2], ARGV[1]) and 1 or 0
`)

// Remove old queues immediately on startup, including probes not due for
// another ten minutes. Runtime reads and claims also enforce the same cleanup.
func (l *Limiter) discardEmbeddingRecovery(ctx context.Context) error {
	for _, model := range []bool{true, false} {
		prefix := healthKey("")
		if model {
			prefix = modelHealthKey("")
		}
		var cursor uint64
		for {
			keys, next, err := l.redis.Scan(ctx, cursor, prefix+"*", 100).Result()
			if err != nil {
				return err
			}
			for _, key := range keys {
				id := strings.TrimPrefix(key, prefix)
				if model {
					routeID, err := l.redis.HGet(ctx, key, "route_id").Result()
					if err != nil && err != redis.Nil {
						return err
					}
					err = discardEmbeddingModelScript.Run(ctx, l.redis,
						[]string{key, modelProbeDueKey(), routeModelIndexKey(routeID)}, id).Err()
					if err != nil {
						return err
					}
				} else if err := discardEmbeddingRouteScript.Run(ctx, l.redis, []string{key, probeDueKey()}, id).Err(); err != nil {
					return err
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}
	return nil
}
