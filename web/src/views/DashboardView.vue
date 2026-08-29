<script setup lang="ts">
import AppShell from '../components/AppShell.vue'
</script>

<template>
  <AppShell>
    <section
      class="hero"
      aria-labelledby="dashboard-title"
    >
      <div>
        <div class="eyebrow">
          控制台
        </div>
        <h1 id="dashboard-title">
          欢迎使用 Bablo
        </h1>
        <p class="muted">
          统一管理多模型访问、路由、Usage 和账务。
        </p>
      </div>
      <span class="status-pill">架构初始化中</span>
    </section>

    <section
      class="empty-card"
      aria-label="Dashboard 空状态"
    >
      <div class="empty-icon">
        ✦
      </div>
      <h2>Dashboard 即将就绪</h2>
      <p class="muted">
        数据层和认证完成后，这里将展示余额、请求、Token、费用和常用模型。
      </p>
    </section>
  </AppShell>
</template>
