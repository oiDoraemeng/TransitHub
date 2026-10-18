<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronDown, Clock3, FlaskConical, Trash2, TriangleAlert } from 'lucide-vue-next'
import type { ProxyModelHealth } from '../types/modelProxy'

const props = defineProps<{ health?: ProxyModelHealth[]; disabled?: boolean }>()
const emit = defineEmits<{ test: [model: ProxyModelHealth]; remove: [model: ProxyModelHealth] }>()
const { t, locale } = useI18n()
const models = computed(() => [...(props.health ?? [])].sort((a, b) => a.modelId.localeCompare(b.modelId) || a.protocol.localeCompare(b.protocol)))
const protocolLabel = (protocol: string) => ({
  '/v1/chat/completions': 'Chat', '/v1/responses': 'Responses', '/v1/responses/compact': 'Compact', '/v1/messages': 'Messages',
  gemini: 'Gemini',
}[protocol] ?? protocol)
const probeTime = (value: string) => new Intl.DateTimeFormat(locale.value, {
  month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
}).format(new Date(value))
</script>

<template>
  <details v-if="models.length" class="model-health mt-1.5 max-w-[260px] rounded-lg border border-warning/25 bg-warning/5 text-xs">
    <summary class="flex cursor-pointer list-none items-center gap-1.5 px-2 py-1.5 text-warning focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary" :title="t('admin.modelProxy.circuit.modelHelp')">
      <TriangleAlert class="h-3.5 w-3.5 shrink-0" />
      <span class="shrink-0 font-medium">{{ t('admin.modelProxy.circuit.unavailable') }}</span>
      <span class="max-w-40 truncate font-mono" :title="models[0]!.modelId">{{ models[0]!.modelId }}</span>
      <span v-if="models.length > 1" class="shrink-0 rounded bg-warning/10 px-1.5 py-0.5 tabular-nums">+{{ models.length - 1 }}</span>
      <ChevronDown class="health-chevron ml-auto h-3.5 w-3.5 shrink-0 transition-transform" />
    </summary>
    <div class="border-t border-warning/20">
      <div class="max-h-60 divide-y divide-warning/15 overflow-y-auto">
        <div v-for="model in models" :key="`${model.modelId}:${model.protocol}`" class="px-2.5 py-2">
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0"><div class="break-all font-mono font-medium text-foreground">{{ model.modelId }}</div><span class="mt-1 inline-block rounded bg-surface px-1.5 py-0.5 text-[10px] text-muted-foreground">{{ protocolLabel(model.protocol) }}</span></div>
            <div class="flex shrink-0 items-center">
              <button type="button" class="rounded-md p-1.5 text-primary transition-colors hover:bg-primary/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:opacity-40" :disabled="disabled" :title="t('admin.modelProxy.manualTest.action')" :aria-label="`${t('admin.modelProxy.manualTest.action')} ${model.modelId}`" @click="emit('test', model)"><FlaskConical class="h-4 w-4" /></button>
              <button v-if="!model.probeDisabled" type="button" class="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary" :title="t('admin.modelProxy.circuit.removeModel')" :aria-label="`${t('admin.modelProxy.circuit.removeModel')} ${model.modelId}`" @click="emit('remove', model)"><Trash2 class="h-4 w-4" /></button>
            </div>
          </div>
          <div v-if="model.probeDisabled" class="mt-1.5 rounded bg-surface px-2 py-1 text-[11px] text-muted-foreground">{{ t('admin.modelProxy.circuit.removedModel') }}</div>
          <div v-else-if="model.nextProbeAt" class="mt-1.5 flex items-center gap-1 text-[11px] text-muted-foreground"><Clock3 class="h-3 w-3 shrink-0" />{{ t('admin.modelProxy.circuit.nextProbe', { time: probeTime(model.nextProbeAt) }) }}</div>
        </div>
      </div>
      <p v-if="models.some(model => !model.probeDisabled)" class="px-2.5 py-2 text-[11px] leading-4 text-muted-foreground">{{ t('admin.modelProxy.circuit.modelSchedule') }}</p>
    </div>
  </details>
</template>

<style scoped>
summary::-webkit-details-marker { display: none; }
details[open] .health-chevron { transform: rotate(180deg); }
</style>
