import { createRouter, createWebHistory } from 'vue-router'

import DashboardView from './views/DashboardView.vue'
import LoginView from './views/LoginView.vue'
import NotFoundView from './views/NotFoundView.vue'
import { api } from './lib/api'


export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardView },
    { path: '/login', name: 'login', component: LoginView },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: NotFoundView },
  ],
})

router.beforeEach(async (to) => {
  if (to.name === 'login' || to.name === 'not-found') return true
  try {
    const response = await api.get<{ session: { mfa_required: boolean } }>('/api/v1/auth/session')
    if (response.session.mfa_required) return { name: 'login' }
    return true
  } catch {
    return { name: 'login' }
  }
})
