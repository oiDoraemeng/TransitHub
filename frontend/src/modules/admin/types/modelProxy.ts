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
  keyPreview: string
  modelCount: number
  cleanupPending: number
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
  enabled: boolean
}

export interface SmartGroupInput {
  name: string
  memberKeys: string[]
  enabled: boolean
}

export interface KeyResponse {
  key: string
  preview: string
}

export interface CleanupStatus {
  pending: number
  retrying: number
  lastError: string
}

export type ProxySite = UpstreamSiteResponse
export type ProxyGroup = UpstreamGroupInfo
