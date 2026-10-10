import type { UpstreamGroupInfo, UpstreamSiteResponse } from './upstream'

export interface ProxyRoute {
  id: string
  name: string
  siteId: string
  siteName: string
  groupId: string
  groupName: string
  concurrencyLimit: number
  activeConcurrency: number
  enabled: boolean
  proxyId: string
  proxyName: string
  keyPreview: string
  modelCount: number
  cleanupPending: number
  streamOnly?: boolean
  minInputTokens?: number
  requestsPerMinute?: number
  priority?: number
  useProvidedKey?: boolean
  upstreamKeyPreview?: string
  keywordCheckEnabled?: boolean
  keywordMatchMode?: 'skip_on_match' | 'require_match'
  excludedKeywords?: string[]
  modelMappingEnabled?: boolean
  modelMapping?: Record<string, string>
  syncKeyDeleteEnabled?: boolean
  syncKeyDeleteDelayMs?: number
  modelSyncedAt: string | null
  modelSyncError: string
  createdAt: string
  updatedAt: string
}

export interface ProxyModel {
  id: string
  object?: string
  owned_by?: string
  effectiveConcurrency: number
}

export interface ProxySmartGroup {
  id: string
  name: string
  enabled: boolean
  keyPreview: string
  totalConcurrency: number
  members: ProxyRoute[]
  models: ProxyModel[]
  createdAt: string
  updatedAt: string
}

export interface ProxyRouteInput {
  name: string
  siteId: string
  groupId: string
  groupName: string
  concurrencyLimit: number
  proxyId: string
  enabled: boolean
}

export interface ModelEgressProxy {
  id: string
  name: string
  protocol: string
  address: string
  enabled: boolean
  routeCount: number
  lastTestStatus: 'untested' | 'healthy' | 'failed'
  lastTestLatencyMs: number | null
  lastTestExitIp: string
  lastTestError: string
  lastTestedAt: string | null
  createdAt: string
  updatedAt: string
}

export interface ModelEgressProxyInput {
  name: string
  url?: string
  enabled: boolean
}

export interface ModelEgressProxyTestResult {
  success: boolean
  latencyMs: number
  exitIp: string
  message: string
}

export interface SmartGroupInput {
  name: string
  memberKeys?: string[]
  routeIds?: string[]
  enabled: boolean
}

export interface SmartGroupMemberPolicyInput {
  streamOnly: boolean
  minInputTokens: number
  requestsPerMinute: number
  priority: number
  modelMappingEnabled: boolean
  modelMapping: Record<string, string>
  useProvidedKey: boolean
  upstreamKey?: string
  keywordCheckEnabled: boolean
  keywordMatchMode: 'skip_on_match' | 'require_match'
  excludedKeywords: string[]
  syncKeyDeleteEnabled: boolean
  syncKeyDeleteDelayMs: number
}

export interface KeyResponse {
  key: string
  preview: string
}

export interface CleanupStatus {
  pending: number
  retrying: number
  processing: number
  lastError: string
}

export type ProxySite = UpstreamSiteResponse
export type ProxyGroup = UpstreamGroupInfo
