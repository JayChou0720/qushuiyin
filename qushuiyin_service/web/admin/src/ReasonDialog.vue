<script setup lang="ts">
import { ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { errorText } from './api'
const visible = defineModel<boolean>({ required: true })
const props = defineProps<{ title: string; save: (reason: string) => Promise<unknown>; disabled?: boolean; success?: string }>()
const emit = defineEmits<{ saved: [] }>()
const reason = ref('')
const busy = ref(false)
const error = ref('')
watch(visible, () => { reason.value = ''; error.value = '' })
async function submit() {
  if (busy.value || props.disabled || !reason.value.trim()) return
  busy.value = true
  error.value = ''
  try {
    await props.save(reason.value.trim())
    ElMessage.success(props.success || '已保存')
    visible.value = false
    emit('saved')
  } catch (e) {
    error.value = errorText(e)
  } finally { busy.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" :title="title" width="min(580px, 94vw)" :close-on-click-modal="false"
    :show-close="!busy" :close-on-press-escape="!busy" destroy-on-close>
    <el-form label-position="top" :disabled="busy" @submit.prevent="submit">
      <slot />
      <el-form-item label="变更原因" required>
        <el-input v-model="reason" type="textarea" :rows="2" maxlength="500" show-word-limit />
      </el-form-item>
      <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
      <div class="dialog-actions">
        <el-button :disabled="busy" @click="visible = false">取消</el-button>
        <el-button type="primary" native-type="submit" :loading="busy" :disabled="disabled || !reason.trim()">确认</el-button>
      </div>
    </el-form>
  </el-dialog>
</template>
