<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError, api } from '../lib/api'

const router = useRouter()
const email = ref('')
const password = ref('')
const mfaCode = ref('')
const mfaRequired = ref(false)
const errorMessage = ref('')
const submitting = ref(false)

type SessionResponse = {
  session: { mfa_required: boolean }
}

async function submit() {
  errorMessage.value = ''
  submitting.value = true
  try {
    if (mfaRequired.value) {
      await api.post<SessionResponse>('/api/v1/auth/mfa/verify', { code: mfaCode.value })
      await router.push('/')
      return
    }
    const response = await api.post<SessionResponse>('/api/v1/auth/login', {
      email: email.value,
      password: password.value,
    })
    password.value = ''
    if (response.session.mfa_required) {
      mfaRequired.value = true
      return
    }
    await router.push('/')
  } catch (error) {
    errorMessage.value = error instanceof ApiError
      ? error.message
      : '登录失败，请稍后重试。'
  } finally {
    submitting.value = false
  }
}

onMounted(async () => {
  try {
    const response = await api.get<SessionResponse>('/api/v1/auth/session')
    if (response.session.mfa_required) {
      mfaRequired.value = true
      return
    }
    await router.push('/')
  } catch {
    // No active session: remain on the login form.
  }
})
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
        {{ mfaRequired ? '完成多因素认证' : '登录控制台' }}
      </h1>
      <p class="muted">
        管理 API Key、模型访问权限、Usage 与钱包。
      </p>
      <form @submit.prevent="submit">
        <template v-if="!mfaRequired">
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
        </template>
        <label v-else>
          TOTP 或恢复码
          <input
            v-model="mfaCode"
            type="text"
            autocomplete="one-time-code"
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
          {{ submitting ? '提交中…' : (mfaRequired ? '验证' : '登录') }}
        </button>
      </form>
      <p class="hint">
        Session、CSRF 与管理员 MFA 已由 Bablo 服务端校验。
      </p>
    </section>
  </main>
</template>
