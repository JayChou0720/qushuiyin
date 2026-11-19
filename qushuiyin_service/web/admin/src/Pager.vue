<script setup lang="ts">
defineProps<{ page: number; hasMore?: boolean; loading?: boolean }>()
defineEmits<{ previous: []; next: [] }>()
</script>

<template>
  <div class="pager">
    <el-button :disabled="page === 0 || loading" @click="$emit('previous')">上一页</el-button>
    <span>第 {{ page + 1 }} 页</span>
    <el-button :disabled="!hasMore || loading" @click="$emit('next')">下一页</el-button>
  </div>
</template>
