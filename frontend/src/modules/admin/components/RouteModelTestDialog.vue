<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Check, FlaskConical, Loader2, Play, RefreshCw, X } from 'lucide-vue-next'
import Button from '@/components/ui/button/Button.vue'
import { listRouteTestModels, restoreRouteScheduling, testRouteModel } from '../api/modelProxy'
import type { ProxyModel, ProxyRoute, ProxySmartGroup, RouteModelTestResult, RouteTestProtocol } from '../types/modelProxy'

const props = defineProps<{ route: ProxyRoute; group?: ProxySmartGroup; initialModel?: string; initialProtocol?: RouteTestProtocol }>()
const emit = defineEmits<{ close: []; restored: [] }>()
const { t } = useI18n()
const models = ref<ProxyModel[]>([])
const modelId = ref('')
const selectedModel = ref('')
const customModel = '__custom_model__'
const protocol = ref<RouteTestProtocol>('chat')
const loadingModels = ref(false)
const testing = ref(false)
const restoring = ref(false)
const result = ref<RouteModelTestResult | null>(null)
const errorMessage = ref('')
const controller = new AbortController()
const busy = computed(() => loadingModels.value || testing.value || restoring.value)
const friendlyError = (error: unknown) => {
  const message = error instanceof Error ? error.message : 'admin.modelProxy.errors.request'
  return message.startsWith('admin.') ? t(message) : message
}

watch(modelId, model => {
  const name = model.toLowerCase()
  protocol.value = name.includes('claude') ? 'messages' : name.includes('gemini') ? 'gemini'
    : name.includes('gpt-5') || name.includes('codex') ? 'responses' : 'chat'
})
watch([modelId, protocol], () => { result.value = null; errorMessage.value = '' })
watch(selectedModel, model => { modelId.value = model === customModel ? '' : model })

const loadModels = async (refresh = false) => {
  if (busy.value) return
  loadingModels.value = true
  errorMessage.value = ''
  if (refresh) result.value = null
  try {
    models.value = await listRouteTestModels(props.route.id, props.group?.id, refresh, controller.signal)
    if (!selectedModel.value) selectedModel.value = models.value[0]?.id ?? customModel
    else if (selectedModel.value !== customModel && !models.value.some(model => model.id === selectedModel.value)) {
      selectedModel.value = models.value[0]?.id ?? customModel
    }
  } catch (error) {
    if (!controller.signal.aborted) errorMessage.value = friendlyError(error)
  } finally { loadingModels.value = false }
}

const runTest = async () => {
  if (busy.value || !modelId.value.trim()) return
  testing.value = true
  result.value = null
  errorMessage.value = ''
  try {
    result.value = await testRouteModel(props.route.id, { groupId: props.group?.id, modelId: modelId.value.trim(), protocol: protocol.value }, controller.signal)
  } catch (error) {
    if (!controller.signal.aborted) errorMessage.value = friendlyError(error)
  } finally { testing.value = false }
}

const restore = async () => {
  if (busy.value || !result.value?.success || !result.value.recoveryToken) return
  restoring.value = true
  errorMessage.value = ''
  try {
    await restoreRouteScheduling(props.route.id, { groupId: props.group?.id, token: result.value.recoveryToken }, controller.signal)
    emit('restored')
  } catch (error) {
    result.value = null
    if (!controller.signal.aborted) errorMessage.value = friendlyError(error)
  } finally { restoring.value = false }
}

const close = () => { if (!busy.value) emit('close') }
onMounted(async () => {
  await loadModels()
  if (props.initialModel) {
    if (!models.value.some(model => model.id === props.initialModel)) {
      models.value.push({ id: props.initialModel, effectiveConcurrency: 0 })
    }
    selectedModel.value = props.initialModel
    await nextTick()
    if (props.initialProtocol) protocol.value = props.initialProtocol
  }
})
onBeforeUnmount(() => controller.abort())
</script>

<template>
  <Teleport to="body">
    <div class="fixed inset-0 z-[80] flex items-center justify-center bg-background/75 p-4 backdrop-blur-sm" @click.self="close" @keydown.esc="close">
      <form role="dialog" aria-modal="true" aria-labelledby="route-model-test-title" class="flex max-h-[90vh] w-full max-w-xl flex-col overflow-hidden rounded-2xl border border-border bg-surface-elevated shadow-2xl" @submit.prevent="runTest">
        <div class="flex items-start justify-between gap-4 border-b border-border px-5 py-4">
          <div><h3 id="route-model-test-title" class="font-semibold">{{ t('admin.modelProxy.manualTest.title') }}</h3><p class="mt-1 text-sm text-muted-foreground">{{ route.name }}<span v-if="group"> · {{ group.name }}</span></p></div>
          <button type="button" :disabled="busy" :aria-label="t('admin.modelProxy.cancel')" class="disabled:opacity-50" @click="close"><X class="h-5 w-5" /></button>
        </div>
        <div class="min-h-0 space-y-4 overflow-y-auto p-5">
          <p class="text-sm text-muted-foreground">{{ t(group ? 'admin.modelProxy.manualTest.memberHelp' : 'admin.modelProxy.manualTest.routeHelp') }}</p>
          <div class="grid gap-2 text-sm font-medium">
            <span class="flex items-center justify-between gap-2">{{ t('admin.modelProxy.manualTest.model') }}<button type="button" class="inline-flex items-center gap-1 text-xs text-primary disabled:opacity-50" :disabled="busy" @click="loadModels(true)"><Loader2 v-if="loadingModels" class="h-3.5 w-3.5 animate-spin" /><RefreshCw v-else class="h-3.5 w-3.5" />{{ t('admin.modelProxy.manualTest.refreshModels') }}</button></span>
            <select v-model="selectedModel" :disabled="busy" :aria-label="t('admin.modelProxy.manualTest.model')" class="border border-border bg-background px-3 py-2 font-normal outline-none focus:border-primary">
              <option v-if="!selectedModel" value="" disabled>{{ t('admin.modelProxy.manualTest.modelPlaceholder') }}</option>
              <option v-for="model in models" :key="model.id" :value="model.id">{{ model.id }}</option>
              <option :value="customModel">{{ t('admin.modelProxy.manualTest.customModel') }}</option>
            </select>
            <input v-if="selectedModel === customModel" v-model="modelId" required :disabled="busy" :aria-label="t('admin.modelProxy.manualTest.customModel')" :placeholder="t('admin.modelProxy.manualTest.modelPlaceholder')" autocomplete="off" class="border border-border bg-background px-3 py-2 font-normal outline-none focus:border-primary" />
            <small class="font-normal text-muted-foreground">{{ t('admin.modelProxy.manualTest.modelCount', { count: models.length }) }}</small>
          </div>
          <label class="grid gap-2 text-sm font-medium"><span>{{ t('admin.modelProxy.manualTest.protocol') }}</span><select v-model="protocol" :disabled="busy" class="border border-border bg-background px-3 py-2 font-normal outline-none focus:border-primary"><option value="chat">OpenAI Chat Completions</option><option value="responses">OpenAI Responses</option><option value="compact">OpenAI Responses Compact</option><option value="messages">Anthropic Messages</option><option value="gemini">Gemini</option></select></label>
          <div v-if="result" role="status" aria-live="polite" class="border px-3 py-3 text-sm" :class="result.success ? 'border-signal/30 bg-signal/10 text-signal' : 'border-destructive/30 bg-destructive/10 text-destructive'">
            <div class="flex items-center gap-2 font-medium"><Check v-if="result.success" class="h-4 w-4" />{{ t(result.message) }}</div>
            <p class="mt-1 text-xs">{{ t('admin.modelProxy.manualTest.latency', { ms: result.latencyMs }) }}<span v-if="result.statusCode"> · HTTP {{ result.statusCode }}</span></p>
            <p v-if="result.recoveryToken" class="mt-2 text-xs">{{ t('admin.modelProxy.manualTest.validFor') }}</p>
          </div>
          <p v-if="errorMessage" role="alert" class="text-sm text-destructive">{{ errorMessage }}</p>
        </div>
        <div class="flex flex-wrap justify-end gap-2 border-t border-border px-5 py-3">
          <Button type="button" variant="ghost" :disabled="busy" @click="close">{{ t('admin.modelProxy.cancel') }}</Button>
          <Button type="submit" variant="secondary" :disabled="busy || !modelId.trim()" :title="t(testing ? 'admin.modelProxy.manualTest.testing' : 'admin.modelProxy.manualTest.run')" :aria-label="t(testing ? 'admin.modelProxy.manualTest.testing' : 'admin.modelProxy.manualTest.run')"><Loader2 v-if="testing" class="h-4 w-4 animate-spin" /><FlaskConical v-else class="h-4 w-4" /></Button>
          <Button type="button" :disabled="busy || !result?.success || !result.recoveryToken" @click="restore"><Loader2 v-if="restoring" class="h-4 w-4 animate-spin" /><Play v-else class="h-4 w-4" />{{ t('admin.modelProxy.manualTest.restore') }}</Button>
        </div>
      </form>
    </div>
  </Teleport>
</template>
