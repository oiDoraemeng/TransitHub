<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from '@/components/ui/button/Button.vue'
import { getUsageCorrection, saveUsageCorrection } from '../api/modelProxy'

const { t } = useI18n()
const settings = reactive({ enabled: false, allowancePercent: 10 })
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const saved = ref(false)
const valid = computed(() => Number.isInteger(settings.allowancePercent) && settings.allowancePercent >= 0 && settings.allowancePercent <= 1000)
const explainError = (value: unknown) => {
  const message = value instanceof Error ? value.message : 'admin.modelProxy.errors.request'
  return message.startsWith('admin.') ? t(message) : message
}
const load = async () => {
  loading.value = true
  error.value = ''
  try { Object.assign(settings, await getUsageCorrection()) } catch (e) { error.value = explainError(e) }
  finally { loading.value = false }
}
const save = async () => {
  if (!valid.value || loading.value || saving.value) return
  saving.value = true; error.value = ''; saved.value = false
  try { Object.assign(settings, await saveUsageCorrection({ ...settings })); saved.value = true }
  catch (e) { error.value = explainError(e) }
  finally { saving.value = false }
}
onMounted(load)
</script>

<template>
  <section class="mb-4 rounded-xl border border-border bg-surface px-4 py-3" aria-labelledby="usage-correction-title">
    <form class="space-y-3" @submit.prevent="save" @input="saved = false" @change="saved = false">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 id="usage-correction-title" class="text-sm font-semibold">{{ t('admin.modelProxy.usageCorrection.title') }}</h3>
          <p class="mt-1 text-xs text-muted-foreground">{{ t('admin.modelProxy.usageCorrection.description') }}</p>
        </div>
        <label class="inline-flex cursor-pointer items-center gap-2 text-sm">
          <input v-model="settings.enabled" type="checkbox" role="switch" :disabled="loading || saving || !!error" class="h-4 w-4 accent-primary" />
          {{ t('admin.modelProxy.usageCorrection.enable') }}
        </label>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <label for="input-allowance" class="text-sm">{{ t('admin.modelProxy.usageCorrection.allowance') }}</label>
        <input id="input-allowance" v-model.number="settings.allowancePercent" type="number" min="0" max="1000" step="1" required :disabled="loading || saving" :aria-invalid="!valid" class="h-8 w-24 rounded-md border border-border bg-background px-2 text-sm" />
        <span class="text-sm">%</span>
        <button v-for="percent in [0, 10, 20, 50]" :key="percent" type="button" :disabled="loading || saving" class="rounded-md border px-2 py-1 text-xs" :class="settings.allowancePercent === percent ? 'border-primary text-primary' : 'border-border text-muted-foreground'" @click="settings.allowancePercent = percent; saved = false">{{ percent }}%</button>
        <Button type="submit" size="sm" :disabled="loading || saving || !valid || !!error">{{ saving ? t('admin.modelProxy.usageCorrection.saving') : t('admin.modelProxy.save') }}</Button>
        <span v-if="saved" role="status" class="text-xs text-signal">{{ t('admin.modelProxy.usageCorrection.saved') }}</span>
      </div>
      <p class="text-xs leading-relaxed text-muted-foreground">{{ t('admin.modelProxy.usageCorrection.help') }}</p>
      <div v-if="error" role="alert" class="flex items-center gap-2 text-xs text-destructive">{{ error }} <button type="button" class="underline" @click="load">{{ t('admin.modelProxy.usageCorrection.retry') }}</button></div>
    </form>
  </section>
</template>
