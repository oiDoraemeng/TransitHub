import { authUnauthorizedErrorKey, getAccessToken, handleAuthExpired, isUnauthorizedApiResponse } from '@/modules/auth/api/auth'
import type {
  CleanupStatus,
  KeyResponse,
  ModelEgressProxy,
  ModelEgressProxyInput,
  ModelEgressProxyTestResult,
  ProxyGroup,
  ProxyCustomKey,
  ProxyRoute,
  ProxyRouteInput,
  ProxyModel,
  RouteModelTestResult,
  RouteTestProtocol,
  ProxySite,
  ProxySmartGroup,
  SmartGroupInput,
  SmartGroupMemberPolicyInput,
} from '../types/modelProxy'

const apiBaseUrl = import.meta.env.VITE_API_BASE_URL ?? '/api'
const endpoint = (path: string) => `${apiBaseUrl.replace(/\/$/, '')}${path}`

type ErrorPayload = { message?: string }

const request = async <T>(path: string, options: RequestInit = {}): Promise<T> => {
  const token = getAccessToken()
  let response: Response
  try {
    response = await fetch(endpoint(path), {
      ...options,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(options.headers ?? {}),
      },
    })
  } catch {
    throw new Error('admin.modelProxy.errors.network')
  }
  const text = await response.text()
  let payload: T & ErrorPayload = {} as T & ErrorPayload
  if (text) {
    try { payload = JSON.parse(text) as T & ErrorPayload } catch { throw new Error('admin.modelProxy.errors.request') }
  }
  if (!response.ok) {
    if (isUnauthorizedApiResponse(response.status, payload)) {
      handleAuthExpired()
      throw new Error(authUnauthorizedErrorKey)
    }
    throw new Error(payload.message || 'admin.modelProxy.errors.request')
  }
  return payload
}

export const listProxyRoutes = () => request<ProxyRoute[]>('/proxy-routes')
export const listProxySites = () => request<ProxySite[]>('/proxy-routes/sites')
export const listProxyGroups = (siteId: string) => request<ProxyGroup[]>(`/proxy-routes/sites/${encodeURIComponent(siteId)}/groups`)
export const createProxyRoute = (input: ProxyRouteInput) => request<{ route: ProxyRoute; key: string }>('/proxy-routes', { method: 'POST', body: JSON.stringify(input) })
export const updateProxyRoute = (id: string, input: Partial<ProxyRouteInput>) => request<ProxyRoute>(`/proxy-routes/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
export const deleteProxyRoute = (id: string) => request<{ success: boolean }>(`/proxy-routes/${id}`, { method: 'DELETE' })
export const revealProxyRouteKey = (id: string) => request<KeyResponse>(`/proxy-routes/${id}/key`)
export const rotateProxyRouteKey = (id: string) => request<KeyResponse>(`/proxy-routes/${id}/rotate-key`, { method: 'POST' })
export const getProxyCleanupStatus = () => request<CleanupStatus>('/proxy-routes/cleanup-status')

export const listRouteTestModels = (id: string, groupId = '', refresh = false, signal?: AbortSignal) => {
  const query = new URLSearchParams({ groupId, refresh: String(refresh) })
  return request<ProxyModel[]>(`/proxy-routes/${encodeURIComponent(id)}/test-models?${query}`, { signal })
}
export const testRouteModel = (id: string, input: { groupId?: string; modelId: string; protocol: RouteTestProtocol }, signal?: AbortSignal) =>
  request<RouteModelTestResult>(`/proxy-routes/${encodeURIComponent(id)}/test`, { method: 'POST', body: JSON.stringify(input), signal })
export const restoreRouteScheduling = (id: string, input: { groupId?: string; token: string }, signal?: AbortSignal) =>
  request<{ success: boolean }>(`/proxy-routes/${encodeURIComponent(id)}/restore-scheduling`, { method: 'POST', body: JSON.stringify(input), signal })
export const removeRouteModel = (id: string, input: { modelId: string; protocol: string }) =>
  request<{ success: boolean }>(`/proxy-routes/${encodeURIComponent(id)}/remove-model`, { method: 'POST', body: JSON.stringify(input) })

export const listModelEgressProxies = () => request<ModelEgressProxy[]>('/model-proxies')
export const createModelEgressProxy = (input: ModelEgressProxyInput & { url: string }) => request<ModelEgressProxy>('/model-proxies', { method: 'POST', body: JSON.stringify(input) })
export const updateModelEgressProxy = (id: string, input: Partial<ModelEgressProxyInput>) => request<ModelEgressProxy>(`/model-proxies/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
export const deleteModelEgressProxy = (id: string) => request<{ success: boolean }>(`/model-proxies/${id}`, { method: 'DELETE' })
export const testModelEgressProxy = (id: string) => request<ModelEgressProxyTestResult>(`/model-proxies/${id}/test`, { method: 'POST' })

export const listProxySmartGroups = () => request<ProxySmartGroup[]>('/proxy-smart-groups')
export const createProxySmartGroup = (input: SmartGroupInput) => request<{ group: ProxySmartGroup; key: string }>('/proxy-smart-groups', { method: 'POST', body: JSON.stringify(input) })
export const updateProxySmartGroup = (id: string, input: { name?: string; keywords?: string[]; enabled?: boolean }) => request<ProxySmartGroup>(`/proxy-smart-groups/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
export const deleteProxySmartGroup = (id: string) => request<{ success: boolean }>(`/proxy-smart-groups/${id}`, { method: 'DELETE' })
export const addProxySmartGroupMember = (id: string, input: { routeId?: string; entryKey?: string }) => request<{ success: boolean }>(`/proxy-smart-groups/${id}/members`, { method: 'POST', body: JSON.stringify(input) })
export const updateProxySmartGroupMemberPolicy = (id: string, routeId: string, input: Partial<SmartGroupMemberPolicyInput>) => request<{ success: boolean }>(`/proxy-smart-groups/${id}/members/${routeId}`, { method: 'PATCH', body: JSON.stringify(input) })
export const removeProxySmartGroupMember = (id: string, routeId: string) => request<{ success: boolean }>(`/proxy-smart-groups/${id}/members/${routeId}`, { method: 'DELETE' })
export const revealProxySmartGroupKey = (id: string) => request<KeyResponse>(`/proxy-smart-groups/${id}/key`)
export const rotateProxySmartGroupKey = (id: string) => request<KeyResponse>(`/proxy-smart-groups/${id}/rotate-key`, { method: 'POST' })
export const updateProxySmartGroupCustomKeys = (id: string, input: { addKeys: string[]; removeKeyIds: string[] }) => request<ProxyCustomKey[]>(`/proxy-smart-groups/${id}/custom-keys`, { method: 'PATCH', body: JSON.stringify(input) })
export const revealProxySmartGroupCustomKey = (id: string, keyId: string) => request<KeyResponse>(`/proxy-smart-groups/${id}/custom-keys/${keyId}`)
