<script setup lang="ts">
const value = defineModel<string>({ required: true })
defineProps<{ label: string; options: Record<string, string> }>()
</script>
<template>
  <el-form-item :label="label">
    <el-select v-model="value" clearable :value-on-clear="''" placeholder="全部" class="select-filter">
      <el-option v-for="(name, key) in options" :key="key" :label="name" :value="key" />
    </el-select>
  </el-form-item>
</template>
