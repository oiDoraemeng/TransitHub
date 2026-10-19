package model_proxy

import (
	"context"

	"github.com/redis/go-redis/v9"
)

const compactPath = "/v1/responses/compact"

// Keep known Compact failures excluded, but discard every automatic probe and
// fence workers claimed by an older version. Manual tests can still recover it.
const compactModelHealthLua = `
local function stopCompactModelProbes(key, dueKey, id)
  local compact = redis.call('HGET', key, 'protocol') == '/v1/responses/compact'
  local encoded = redis.call('HGET', key, 'probe_spec')
  if not compact and encoded then
    local ok, spec = pcall(cjson.decode, encoded)
    compact = ok and type(spec) == 'table' and spec.path == '/v1/responses/compact'
  end
  if not compact then return false end
  if redis.call('HGET', key, 'automatic_probe_disabled') ~= '1'
     or encoded or redis.call('HEXISTS', key, 'probe_token') == 1 or redis.call('ZSCORE', dueKey, id) then
    redis.call('HINCRBY', key, 'epoch', 1)
  end
  redis.call('HSET', key, 'automatic_probe_disabled', 1)
  redis.call('HDEL', key, 'probe_spec', 'probe_token')
  redis.call('ZREM', dueKey, id)
  return true
end
`

var stopCompactModelProbesScript = redis.NewScript(compactModelHealthLua + `
return stopCompactModelProbes(KEYS[1], KEYS[2], ARGV[1]) and 1 or 0
`)

var discardCompactRouteScript = redis.NewScript(routeHealthLua + `
return discardCompactRoute(KEYS[1], KEYS[2], ARGV[1]) and 1 or 0
`)

func (l *Limiter) discardCompactRecovery(ctx context.Context) error {
	return l.discardRecovery(ctx, stopCompactModelProbesScript, discardCompactRouteScript)
}
