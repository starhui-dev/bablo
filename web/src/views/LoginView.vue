<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError, api } from '../lib/api'

const router = useRouter()
const email = ref('')
const password = ref('')
const errorMessage = ref('')
const submitting = ref(false)

async function submit() {
  errorMessage.value = ''
  submitting.value = true
  try {
    await api.post('/api/v1/auth/login', { email: email.value, password: password.value })
    await router.push('/')
  } catch (error) {
    errorMessage.value = error instanceof ApiError
      ? error.message
      : '登录失败，请稍后重试。'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <main class="auth-page">
    <section
      class="auth-card"
      aria-labelledby="login-title"
    >
      <div class="eyebrow">
        Bablo · AI Gateway
      </div>
      <h1 id="login-title">
        登录控制台
      </h1>
      <p class="muted">
        管理 API Key、模型访问权限、Usage 与钱包。
      </p>
      <form @submit.prevent="submit">
        <label>
          邮箱
          <input
            v-model="email"
            type="email"
            autocomplete="username"
            required
          >
        </label>
        <label>
          密码
          <input
            v-model="password"
            type="password"
            autocomplete="current-password"
            required
          >
        </label>
        <p
          v-if="errorMessage"
          class="form-error"
          role="alert"
        >
          {{ errorMessage }}
        </p>
        <button
          class="primary-button"
          type="submit"
          :disabled="submitting"
        >
          {{ submitting ? '登录中…' : '登录' }}
        </button>
      </form>
      <p class="hint">
        认证接口将在 auth 阶段接入；当前页面用于验证前端 shell 和错误处理。
      </p>
    </section>
  </main>
</template>
