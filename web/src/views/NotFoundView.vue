<script setup lang="ts">
import { RouterLink } from 'vue-router'
</script>

<template>
  <main class="error-page">
    <div class="error-code">
      404
    </div>
    <h1>页面不存在</h1>
    <p class="muted">
      你访问的路径不存在，或功能尚未开放。
    </p>
    <RouterLink
      class="primary-button inline-button"
      to="/"
    >
      返回 Dashboard
    </RouterLink>
  </main>
</template>
