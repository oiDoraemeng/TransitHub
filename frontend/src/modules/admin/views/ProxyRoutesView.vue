<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Activity, Check, ChevronDown, ChevronRight, Clipboard, Eye, KeyRound, Layers3,
  Loader2, Pencil, Plus, RefreshCw, Route, Trash2, Users, X,
} from 'lucide-vue-next'
import Button from '@/components/ui/button/Button.vue'
import {
  addProxySmartGroupMember, createProxyRoute, createProxySmartGroup, deleteProxyRoute,
  deleteProxySmartGroup, getProxyCleanupStatus, listProxyGroups, listProxyRoutes,
  listProxySites, listProxySmartGroups, removeProxySmartGroupMember, revealProxyRouteKey,
  revealProxySmartGroupKey, rotateProxyRouteKey, rotateProxySmartGroupKey,
  updateProxyRoute, updateProxySmartGroup,
} from '../api/modelProxy'
import type { CleanupStatus, ProxyGroup, ProxyRoute, ProxyRouteInput, ProxySite, ProxySmartGroup } from '../types/modelProxy'

const { t } = useI18n()
const activeTab = ref<'routes' | 'groups'>('routes')
const routes = ref<ProxyRoute[]>([])
const smartGroups = ref<ProxySmartGroup[]>([])
const sites = ref<ProxySite[]>([])
const availableGroups = ref<ProxyGroup[]>([])
const cleanup = ref<CleanupStatus>({ pending: 0, retrying: 0, lastError: '' })
const loading = ref(true)
const saving = ref(false)
const groupsLoading = ref(false)
const errorMessage = ref('')
const noticeMessage = ref('')
const expandedGroups = ref(new Set<string>())
let pollTimer: number | undefined

const routeModalOpen = ref(false)
const editingRouteId = ref('')
const routeForm = reactive<ProxyRouteInput>({ name: '', siteId: '', groupId: '', groupName: '', concurrencyLimit: 50, enabled: true })

const smartModalOpen = ref(false)
const editingSmartGroupId = ref('')
const smartName = ref('')
const smartEnabled = ref(true)
const selectedRouteIds = ref<string[]>([])
const pastedMemberKeys = ref('')

const memberModalGroup = ref<ProxySmartGroup | null>(null)
const memberKey = ref('')
const keyModal = reactive({ open: false, title: '', key: '' })

const enabledRoutes = computed(() => routes.value.filter(route => route.enabled))

const friendlyError = (error: unknown) => {
  const message = error instanceof Error ? error.message : 'admin.modelProxy.errors.request'
  return message.startsWith('admin.') ? t(message) : message
}

const flash = (message: string) => {
  noticeMessage.value = message
  window.setTimeout(() => { if (noticeMessage.value === message) noticeMessage.value = '' }, 3500)
}

const loadAll = async (quiet = false) => {
  if (!quiet) loading.value = true
  errorMessage.value = ''
  try {
    const [routeRows, groupRows, siteRows, cleanupStatus] = await Promise.all([
      listProxyRoutes(), listProxySmartGroups(), listProxySites(), getProxyCleanupStatus(),
    ])
    routes.value = routeRows
    smartGroups.value = groupRows
    sites.value = siteRows
    cleanup.value = cleanupStatus
  } catch (error) {
    errorMessage.value = friendlyError(error)
  } finally {
    loading.value = false
  }
}

const loadGroups = async (siteId: string) => {
  availableGroups.value = []
  if (!siteId) return
  groupsLoading.value = true
  try {
    availableGroups.value = await listProxyGroups(siteId)
  } catch (error) {
    errorMessage.value = friendlyError(error)
  } finally {
    groupsLoading.value = false
  }
}

watch(() => routeForm.siteId, async (siteId, previous) => {
  if (siteId !== previous && routeModalOpen.value) {
    routeForm.groupId = ''
    routeForm.groupName = ''
    await loadGroups(siteId)
  }
})

const openCreateRoute = () => {
  editingRouteId.value = ''
  Object.assign(routeForm, { name: '', siteId: sites.value[0]?.id ?? '', groupId: '', groupName: '', concurrencyLimit: 50, enabled: true })
  routeModalOpen.value = true
  void loadGroups(routeForm.siteId)
}

const openEditRoute = async (route: ProxyRoute) => {
  editingRouteId.value = route.id
  Object.assign(routeForm, { name: route.name, siteId: route.siteId, groupId: route.groupId, groupName: route.groupName, concurrencyLimit: route.concurrencyLimit, enabled: route.enabled })
  routeModalOpen.value = true
  await loadGroups(route.siteId)
  routeForm.groupId = route.groupId
}

const onGroupSelected = () => {
  routeForm.groupName = availableGroups.value.find(group => group.id === routeForm.groupId)?.name ?? routeForm.groupId
}

const submitRoute = async () => {
  saving.value = true
  errorMessage.value = ''
  try {
    if (editingRouteId.value) {
      await updateProxyRoute(editingRouteId.value, { ...routeForm })
      flash(t('admin.modelProxy.notices.routeUpdated'))
    } else {
      const created = await createProxyRoute({ ...routeForm })
      showKey(t('admin.modelProxy.key.newRoute'), created.key)
    }
    routeModalOpen.value = false
    await loadAll(true)
  } catch (error) {
    errorMessage.value = friendlyError(error)
  } finally {
    saving.value = false
  }
}

const toggleRoute = async (route: ProxyRoute) => {
  try {
    await updateProxyRoute(route.id, { enabled: !route.enabled })
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const removeRoute = async (route: ProxyRoute) => {
  if (!window.confirm(t('admin.modelProxy.confirm.deleteRoute', { name: route.name }))) return
  try {
    await deleteProxyRoute(route.id)
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const openCreateSmart = () => {
  editingSmartGroupId.value = ''
  smartName.value = ''
  smartEnabled.value = true
  selectedRouteIds.value = []
  pastedMemberKeys.value = ''
  smartModalOpen.value = true
}

const openEditSmart = (group: ProxySmartGroup) => {
  editingSmartGroupId.value = group.id
  smartName.value = group.name
  smartEnabled.value = group.enabled
  selectedRouteIds.value = group.members.map(member => member.id)
  pastedMemberKeys.value = ''
  smartModalOpen.value = true
}

const submitSmart = async () => {
  saving.value = true
  errorMessage.value = ''
  try {
    if (editingSmartGroupId.value) {
      await updateProxySmartGroup(editingSmartGroupId.value, { name: smartName.value.trim(), enabled: smartEnabled.value })
      flash(t('admin.modelProxy.notices.groupUpdated'))
    } else {
      const selectedKeys = await Promise.all(selectedRouteIds.value.map(id => revealProxyRouteKey(id).then(result => result.key)))
      const pasted = pastedMemberKeys.value.split(/\r?\n|,/).map(value => value.trim()).filter(Boolean)
      const created = await createProxySmartGroup({ name: smartName.value.trim(), enabled: smartEnabled.value, memberKeys: [...new Set([...selectedKeys, ...pasted])] })
      showKey(t('admin.modelProxy.key.newGroup'), created.key)
    }
    smartModalOpen.value = false
    await loadAll(true)
  } catch (error) {
    errorMessage.value = friendlyError(error)
  } finally { saving.value = false }
}

const toggleSmart = async (group: ProxySmartGroup) => {
  try {
    await updateProxySmartGroup(group.id, { enabled: !group.enabled })
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const removeSmart = async (group: ProxySmartGroup) => {
  if (!window.confirm(t('admin.modelProxy.confirm.deleteGroup', { name: group.name }))) return
  try {
    await deleteProxySmartGroup(group.id)
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const submitMember = async () => {
  if (!memberModalGroup.value || !memberKey.value.trim()) return
  saving.value = true
  try {
    await addProxySmartGroupMember(memberModalGroup.value.id, memberKey.value.trim())
    memberModalGroup.value = null
    memberKey.value = ''
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) } finally { saving.value = false }
}

const removeMember = async (group: ProxySmartGroup, route: ProxyRoute) => {
  if (!window.confirm(t('admin.modelProxy.confirm.removeMember', { name: route.name }))) return
  try {
    await removeProxySmartGroupMember(group.id, route.id)
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const showKey = (title: string, key: string) => Object.assign(keyModal, { open: true, title, key })

const revealKey = async (owner: ProxyRoute | ProxySmartGroup, type: 'route' | 'group') => {
  try {
    const result = type === 'route' ? await revealProxyRouteKey(owner.id) : await revealProxySmartGroupKey(owner.id)
    showKey(owner.name, result.key)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const rotateKey = async (owner: ProxyRoute | ProxySmartGroup, type: 'route' | 'group') => {
  if (!window.confirm(t('admin.modelProxy.confirm.rotateKey'))) return
  try {
    const result = type === 'route' ? await rotateProxyRouteKey(owner.id) : await rotateProxySmartGroupKey(owner.id)
    showKey(owner.name, result.key)
    await loadAll(true)
  } catch (error) { errorMessage.value = friendlyError(error) }
}

const copyKey = async () => {
  await navigator.clipboard.writeText(keyModal.key)
  flash(t('admin.modelProxy.notices.copied'))
}

const toggleExpanded = (id: string) => {
  const next = new Set(expandedGroups.value)
  next.has(id) ? next.delete(id) : next.add(id)
  expandedGroups.value = next
}

const formatDate = (value: string | null) => value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'short', timeStyle: 'short' }).format(new Date(value)) : t('admin.modelProxy.pending')

onMounted(() => {
  void loadAll()
  pollTimer = window.setInterval(() => void loadAll(true), 10000)
})
onBeforeUnmount(() => { if (pollTimer) window.clearInterval(pollTimer) })
</script>

<template>
  <div class="min-h-full">
    <section class="border-b border-border/50 bg-surface/40 px-4 py-5 sm:px-6">
      <div class="mx-auto flex max-w-7xl flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div>
          <h2 class="text-xl font-semibold text-foreground">{{ t('admin.modelProxy.title') }}</h2>
          <p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.subtitle') }}</p>
        </div>
        <div class="flex flex-wrap items-center gap-3 text-sm">
          <span class="inline-flex items-center gap-2 text-muted-foreground"><Activity class="h-4 w-4 text-signal" />{{ t('admin.modelProxy.cleanup.pending', { count: cleanup.pending }) }}</span>
          <span v-if="cleanup.retrying" class="inline-flex items-center gap-2 text-warning"><RefreshCw class="h-4 w-4" />{{ t('admin.modelProxy.cleanup.retrying', { count: cleanup.retrying }) }}</span>
          <Button @click="activeTab === 'routes' ? openCreateRoute() : openCreateSmart()"><Plus class="h-4 w-4" />{{ activeTab === 'routes' ? t('admin.modelProxy.routes.add') : t('admin.modelProxy.groups.add') }}</Button>
        </div>
      </div>
    </section>

    <main class="mx-auto max-w-7xl px-4 py-6 sm:px-6">
      <div v-if="errorMessage" class="mb-4 flex items-start justify-between border border-destructive/30 bg-destructive/10 px-4 py-3 text-sm text-destructive">
        <span>{{ errorMessage }}</span><button title="Close" @click="errorMessage = ''"><X class="h-4 w-4" /></button>
      </div>
      <div v-if="noticeMessage" class="mb-4 flex items-center gap-2 border border-signal/30 bg-signal/10 px-4 py-3 text-sm text-signal"><Check class="h-4 w-4" />{{ noticeMessage }}</div>

      <div class="mb-5 inline-flex h-10 items-center border border-border bg-surface p-1" role="tablist">
        <button class="flex h-8 items-center gap-2 px-4 text-sm font-medium" :class="activeTab === 'routes' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground'" @click="activeTab = 'routes'"><Route class="h-4 w-4" />{{ t('admin.modelProxy.routes.tab') }}</button>
        <button class="flex h-8 items-center gap-2 px-4 text-sm font-medium" :class="activeTab === 'groups' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground'" @click="activeTab = 'groups'"><Layers3 class="h-4 w-4" />{{ t('admin.modelProxy.groups.tab') }}</button>
      </div>

      <div v-if="loading" class="flex min-h-64 items-center justify-center text-muted-foreground"><Loader2 class="mr-2 h-5 w-5 animate-spin" />{{ t('admin.modelProxy.loading') }}</div>

      <section v-else-if="activeTab === 'routes'" aria-labelledby="proxy-routes-heading">
        <div class="mb-3 flex items-end justify-between"><div><h3 id="proxy-routes-heading" class="font-semibold">{{ t('admin.modelProxy.routes.heading') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.routes.description') }}</p></div><span class="text-xs text-muted-foreground">{{ routes.length }} {{ t('admin.modelProxy.routes.count') }}</span></div>
        <div v-if="!routes.length" class="border border-dashed border-border py-16 text-center text-sm text-muted-foreground">{{ t('admin.modelProxy.routes.empty') }}</div>
        <div v-else class="overflow-x-auto border border-border">
          <table class="w-full min-w-[920px] text-left text-sm">
            <thead class="bg-surface text-xs uppercase text-muted-foreground"><tr><th class="px-4 py-3">{{ t('admin.modelProxy.table.route') }}</th><th class="px-4 py-3">{{ t('admin.modelProxy.table.binding') }}</th><th class="px-4 py-3">{{ t('admin.modelProxy.table.concurrency') }}</th><th class="px-4 py-3">{{ t('admin.modelProxy.table.models') }}</th><th class="px-4 py-3">Key</th><th class="px-4 py-3">{{ t('admin.modelProxy.table.status') }}</th><th class="px-4 py-3 text-right">{{ t('admin.modelProxy.table.actions') }}</th></tr></thead>
            <tbody class="divide-y divide-border/60">
              <tr v-for="routeItem in routes" :key="routeItem.id" class="hover:bg-surface/40">
                <td class="px-4 py-3"><div class="font-medium">{{ routeItem.name }}</div><div class="mt-1 text-xs text-muted-foreground">{{ formatDate(routeItem.modelSyncedAt) }}</div></td>
                <td class="px-4 py-3"><div>{{ routeItem.siteName }}</div><div class="text-xs text-muted-foreground">{{ routeItem.groupName }}</div></td>
                <td class="px-4 py-3"><span class="font-mono">{{ routeItem.activeConcurrency }} / {{ routeItem.concurrencyLimit }}</span><div class="mt-1 h-1.5 w-24 bg-surface-line"><div class="h-full bg-primary" :style="{ width: `${Math.min(100, routeItem.activeConcurrency / routeItem.concurrencyLimit * 100)}%` }" /></div></td>
                <td class="px-4 py-3"><span>{{ routeItem.modelCount }}</span><span v-if="routeItem.modelSyncError" class="ml-2 text-xs text-warning" :title="routeItem.modelSyncError">{{ t('admin.modelProxy.stale') }}</span></td>
                <td class="px-4 py-3 font-mono text-xs text-muted-foreground">{{ routeItem.keyPreview }}</td>
                <td class="px-4 py-3"><button class="inline-flex items-center gap-2" @click="toggleRoute(routeItem)"><span class="h-2 w-2 rounded-full" :class="routeItem.enabled ? 'bg-signal' : 'bg-muted-foreground'" />{{ routeItem.enabled ? t('admin.modelProxy.enabled') : t('admin.modelProxy.disabled') }}</button><div v-if="routeItem.cleanupPending" class="mt-1 text-xs text-warning">{{ t('admin.modelProxy.cleanup.routePending', { count: routeItem.cleanupPending }) }}</div></td>
                <td class="px-4 py-3"><div class="flex justify-end gap-1"><button class="icon-button" :title="t('admin.modelProxy.actions.reveal')" @click="revealKey(routeItem, 'route')"><Eye /></button><button class="icon-button" :title="t('admin.modelProxy.actions.rotate')" @click="rotateKey(routeItem, 'route')"><RefreshCw /></button><button class="icon-button" :title="t('admin.modelProxy.actions.edit')" @click="openEditRoute(routeItem)"><Pencil /></button><button class="icon-button text-destructive" :title="t('admin.modelProxy.actions.delete')" @click="removeRoute(routeItem)"><Trash2 /></button></div></td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section v-else aria-labelledby="proxy-groups-heading">
        <div class="mb-3"><h3 id="proxy-groups-heading" class="font-semibold">{{ t('admin.modelProxy.groups.heading') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.groups.description') }}</p></div>
        <div v-if="!smartGroups.length" class="border border-dashed border-border py-16 text-center text-sm text-muted-foreground">{{ t('admin.modelProxy.groups.empty') }}</div>
        <div v-else class="divide-y divide-border border border-border">
          <article v-for="group in smartGroups" :key="group.id">
            <div class="grid gap-4 px-4 py-4 md:grid-cols-[minmax(220px,1.3fr)_1fr_1fr_auto] md:items-center">
              <button class="flex min-w-0 items-center gap-3 text-left" @click="toggleExpanded(group.id)"><ChevronDown v-if="expandedGroups.has(group.id)" class="h-4 w-4 shrink-0" /><ChevronRight v-else class="h-4 w-4 shrink-0" /><div class="min-w-0"><div class="truncate font-medium">{{ group.name }}</div><div class="mt-1 truncate font-mono text-xs text-muted-foreground">{{ group.keyPreview }}</div></div></button>
              <div><div class="text-xs text-muted-foreground">{{ t('admin.modelProxy.table.concurrency') }}</div><div class="mt-1 font-mono text-lg">{{ group.totalConcurrency }}</div></div>
              <div class="flex gap-5 text-sm"><span><Users class="mr-1 inline h-4 w-4 text-muted-foreground" />{{ group.members.length }}</span><span><Layers3 class="mr-1 inline h-4 w-4 text-muted-foreground" />{{ group.models.length }}</span><button @click="toggleSmart(group)"><span class="mr-1 inline-block h-2 w-2 rounded-full" :class="group.enabled ? 'bg-signal' : 'bg-muted-foreground'" />{{ group.enabled ? t('admin.modelProxy.enabled') : t('admin.modelProxy.disabled') }}</button></div>
              <div class="flex justify-end gap-1"><button class="icon-button" :title="t('admin.modelProxy.actions.addMember')" @click="memberModalGroup = group"><Plus /></button><button class="icon-button" :title="t('admin.modelProxy.actions.reveal')" @click="revealKey(group, 'group')"><Eye /></button><button class="icon-button" :title="t('admin.modelProxy.actions.rotate')" @click="rotateKey(group, 'group')"><RefreshCw /></button><button class="icon-button" :title="t('admin.modelProxy.actions.edit')" @click="openEditSmart(group)"><Pencil /></button><button class="icon-button text-destructive" :title="t('admin.modelProxy.actions.delete')" @click="removeSmart(group)"><Trash2 /></button></div>
            </div>
            <div v-if="expandedGroups.has(group.id)" class="grid gap-6 border-t border-border bg-surface/30 px-5 py-5 lg:grid-cols-2">
              <div><h4 class="mb-3 text-sm font-medium">{{ t('admin.modelProxy.groups.members') }}</h4><div class="space-y-2"><div v-for="member in group.members" :key="member.id" class="flex items-center justify-between border-b border-border/50 pb-2 text-sm"><div><span class="font-medium">{{ member.name }}</span><span class="ml-2 text-muted-foreground">{{ member.groupName }}</span></div><div class="flex items-center gap-3"><span class="font-mono text-xs">{{ member.activeConcurrency }}/{{ member.concurrencyLimit }}</span><button class="text-destructive" :title="t('admin.modelProxy.actions.removeMember')" @click="removeMember(group, member)"><X class="h-4 w-4" /></button></div></div></div></div>
              <div><h4 class="mb-3 text-sm font-medium">{{ t('admin.modelProxy.groups.models') }}</h4><div class="max-h-56 overflow-y-auto"><div v-for="model in group.models" :key="model.id" class="flex items-center justify-between border-b border-border/50 py-2 text-sm"><span class="font-mono text-xs">{{ model.id }}</span><span class="text-xs text-muted-foreground">{{ t('admin.modelProxy.groups.modelCapacity', { count: model.effectiveConcurrency }) }}</span></div><p v-if="!group.models.length" class="text-sm text-muted-foreground">{{ t('admin.modelProxy.groups.modelsPending') }}</p></div></div>
            </div>
          </article>
        </div>
      </section>
    </main>

    <Teleport to="body">
      <div v-if="routeModalOpen" class="modal-backdrop" @click.self="routeModalOpen = false"><form class="modal-panel" @submit.prevent="submitRoute"><div class="modal-header"><div><h3 class="font-semibold">{{ editingRouteId ? t('admin.modelProxy.routes.edit') : t('admin.modelProxy.routes.add') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.routes.formHelp') }}</p></div><button type="button" @click="routeModalOpen = false"><X class="h-5 w-5" /></button></div><div class="grid gap-4 p-5 sm:grid-cols-2"><label class="field sm:col-span-2"><span>{{ t('admin.modelProxy.form.name') }}</span><input v-model="routeForm.name" required /></label><label class="field"><span>{{ t('admin.modelProxy.form.site') }}</span><select v-model="routeForm.siteId" required><option value="" disabled>{{ t('admin.modelProxy.form.selectSite') }}</option><option v-for="site in sites" :key="site.id" :value="site.id">{{ site.name }}</option></select></label><label class="field"><span>{{ t('admin.modelProxy.form.group') }}</span><select v-model="routeForm.groupId" required :disabled="groupsLoading" @change="onGroupSelected"><option value="" disabled>{{ groupsLoading ? t('admin.modelProxy.loading') : t('admin.modelProxy.form.selectGroup') }}</option><option v-for="group in availableGroups" :key="group.id" :value="group.id">{{ group.name }}</option></select></label><label class="field"><span>{{ t('admin.modelProxy.form.concurrency') }}</span><input v-model.number="routeForm.concurrencyLimit" type="number" min="1" max="100000" required /></label><label class="flex items-center gap-3 self-end pb-2 text-sm"><input v-model="routeForm.enabled" type="checkbox" class="h-4 w-4" />{{ t('admin.modelProxy.form.enabled') }}</label></div><div class="modal-actions"><Button type="button" variant="ghost" @click="routeModalOpen = false">{{ t('admin.modelProxy.cancel') }}</Button><Button type="submit" :disabled="saving"><Loader2 v-if="saving" class="h-4 w-4 animate-spin" />{{ t('admin.modelProxy.save') }}</Button></div></form></div>

      <div v-if="smartModalOpen" class="modal-backdrop" @click.self="smartModalOpen = false"><form class="modal-panel max-w-2xl" @submit.prevent="submitSmart"><div class="modal-header"><div><h3 class="font-semibold">{{ editingSmartGroupId ? t('admin.modelProxy.groups.edit') : t('admin.modelProxy.groups.add') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.groups.formHelp') }}</p></div><button type="button" @click="smartModalOpen = false"><X class="h-5 w-5" /></button></div><div class="space-y-5 p-5"><label class="field"><span>{{ t('admin.modelProxy.form.name') }}</span><input v-model="smartName" required /></label><label class="flex items-center gap-3 text-sm"><input v-model="smartEnabled" type="checkbox" class="h-4 w-4" />{{ t('admin.modelProxy.form.enabled') }}</label><template v-if="!editingSmartGroupId"><fieldset><legend class="mb-2 text-sm font-medium">{{ t('admin.modelProxy.groups.selectRoutes') }}</legend><div class="max-h-48 divide-y divide-border overflow-y-auto border border-border"><label v-for="routeItem in enabledRoutes" :key="routeItem.id" class="flex items-center justify-between gap-3 px-3 py-2 text-sm"><span><span class="font-medium">{{ routeItem.name }}</span><span class="ml-2 text-muted-foreground">{{ routeItem.keyPreview }}</span></span><input v-model="selectedRouteIds" type="checkbox" :value="routeItem.id" class="h-4 w-4" /></label></div></fieldset><label class="field"><span>{{ t('admin.modelProxy.groups.pasteKeys') }}</span><textarea v-model="pastedMemberKeys" rows="3" :placeholder="t('admin.modelProxy.groups.pastePlaceholder')" /></label></template></div><div class="modal-actions"><Button type="button" variant="ghost" @click="smartModalOpen = false">{{ t('admin.modelProxy.cancel') }}</Button><Button type="submit" :disabled="saving || (!editingSmartGroupId && !selectedRouteIds.length && !pastedMemberKeys.trim())"><Loader2 v-if="saving" class="h-4 w-4 animate-spin" />{{ t('admin.modelProxy.save') }}</Button></div></form></div>

      <div v-if="memberModalGroup" class="modal-backdrop" @click.self="memberModalGroup = null"><form class="modal-panel max-w-lg" @submit.prevent="submitMember"><div class="modal-header"><div><h3 class="font-semibold">{{ t('admin.modelProxy.actions.addMember') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ memberModalGroup.name }}</p></div><button type="button" @click="memberModalGroup = null"><X class="h-5 w-5" /></button></div><div class="p-5"><label class="field"><span>{{ t('admin.modelProxy.groups.entryKey') }}</span><input v-model="memberKey" type="password" required placeholder="sk-th-..." /></label></div><div class="modal-actions"><Button type="button" variant="ghost" @click="memberModalGroup = null">{{ t('admin.modelProxy.cancel') }}</Button><Button type="submit" :disabled="saving">{{ t('admin.modelProxy.groups.addMember') }}</Button></div></form></div>

      <div v-if="keyModal.open" class="modal-backdrop" @click.self="keyModal.open = false"><div class="modal-panel max-w-xl"><div class="modal-header"><div><h3 class="font-semibold">{{ keyModal.title }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ t('admin.modelProxy.key.storeSafely') }}</p></div><button @click="keyModal.open = false"><X class="h-5 w-5" /></button></div><div class="p-5"><div class="flex items-center gap-2 border border-border bg-surface p-3"><KeyRound class="h-4 w-4 shrink-0 text-primary" /><code class="min-w-0 flex-1 break-all text-xs">{{ keyModal.key }}</code><button :title="t('admin.modelProxy.actions.copy')" @click="copyKey"><Clipboard class="h-4 w-4" /></button></div></div><div class="modal-actions"><Button @click="copyKey"><Clipboard class="h-4 w-4" />{{ t('admin.modelProxy.actions.copy') }}</Button></div></div></div>
    </Teleport>
  </div>
</template>

<style scoped>
.icon-button { @apply flex h-8 w-8 items-center justify-center text-muted-foreground transition-colors hover:bg-surface-line hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary; }
.icon-button :deep(svg) { @apply h-4 w-4; }
.modal-backdrop { @apply fixed inset-0 z-[80] flex items-center justify-center bg-background/75 p-4 backdrop-blur-sm; }
.modal-panel { @apply max-h-[90vh] w-full max-w-xl overflow-y-auto border border-border bg-surface-elevated shadow-2xl; }
.modal-header { @apply flex items-start justify-between gap-4 border-b border-border px-5 py-4; }
.modal-actions { @apply flex justify-end gap-2 border-t border-border px-5 py-4; }
.field { @apply grid gap-2 text-sm font-medium; }
.field input, .field select, .field textarea { @apply w-full border border-border bg-background px-3 py-2 text-sm font-normal text-foreground outline-none focus:border-primary focus:ring-2 focus:ring-primary/20; }
</style>
