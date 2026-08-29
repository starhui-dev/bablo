<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import { ApiError, api } from '../lib/api'

const router = useRouter()
const signingOut = ref(false)

async function logout() {
  signingOut.value = true
  try {
    await api.post('/api/v1/auth/logout', {})
    await router.push('/login')
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      await router.push('/login')
    }
  } finally {
    signingOut.value = false
  }
}
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
        <button
          class="nav-button"
          type="button"
          :disabled="signingOut"
          @click="logout"
        >
          {{ signingOut ? '退出中…' : '退出登录' }}
        </button>
      </nav>
    </header>
    <main class="page-content">
      <slot />
    </main>
  </div>
</template>
