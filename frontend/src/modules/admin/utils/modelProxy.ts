import type { ProxyModelHealth } from '../types/modelProxy'

export const hasAutomaticModelProbe = (model: ProxyModelHealth) =>
  !model.probeDisabled && !model.automaticProbeDisabled && model.protocol !== '/v1/responses/compact'
