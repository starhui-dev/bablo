<script setup lang="ts">
import { RouterLink } from 'vue-router'
</script>

<template>
  <div class="app-shell">
    <header class="topbar">
      <RouterLink
        class="brand"
        to="/"
      >
        <span class="brand-mark">B</span>
        <span>
          <strong>Bablo</strong>
          <small>AI Gateway</small>
        </span>
      </RouterLink>
      <nav aria-label="主导航">
        <RouterLink to="/">
          Dashboard
        </RouterLink>
        <RouterLink to="/login">
          登录
        </RouterLink>
      </nav>
    </header>
    <main class="page-content">
      <slot />
    </main>
  </div>
</template>
