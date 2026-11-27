<template>
  <div class="share-to-space-panel">
    <div class="share-panel-header">
      <div class="share-panel-header-row">
        <div class="share-panel-titlewrap">
          <h2 class="share-panel-title">{{ $t('organization.share.title') }}</h2>
          <t-popup placement="bottom-start" trigger="hover" overlay-class-name="wk-popover share-hint-popup-overlay"
            :overlay-inner-style="shareHintPopupInnerStyle">
            <button type="button" class="share-hint-trigger-btn" :aria-label="$t('agent.shareScope.title')"
              :title="$t('agent.shareScope.title')">
              <t-icon name="info-circle" size="16px" />
            </button>
            <template #content>
              <div class="share-hint-popover">
                <p class="share-hint-title">{{ $t('agent.shareScope.title') }}</p>
                <p class="share-hint-desc">{{ $t('organization.share.agentShareDesc') }}</p>
                <p v-if="agent?.config" class="share-hint-desc">{{ $t('agent.shareScope.desc') }}</p>
              </div>
            </template>
          </t-popup>
        </div>
      </div>
      <p class="share-panel-desc">{{ $t('organization.share.agentShareDesc') }}</p>
      <t-alert v-if="sharesSkillSecrets" theme="warning" class="share-panel-alert"
        :message="$t('agent.shareScope.skillSecretsWarning')" />
      <!-- 共享被拒且后端说明了缺项：留在面板里，直到共享成功或换了智能体 -->
      <t-alert v-if="refusedIssues.length" theme="error" class="share-panel-alert"
        :message="$t('organization.share.notReadyToShare', { items: refusedIssueSummary })">
        <template v-if="canGoConfigure" #operation>
          <t-button size="small" variant="text" theme="danger" class="share-panel-alert-action"
            @click="goConfigure">{{ $t('organization.share.goConfigure') }}</t-button>
        </template>
      </t-alert>
    </div>

    <div class="share-panel-list-wrap">
      <div class="share-panel-list-header">
        <div class="share-panel-titlewrap">
          <span class="share-panel-list-title">{{ $t('organization.share.sharedTo') }}</span>
          <span class="share-panel-count-badge">{{ filteredShares.length }}</span>
        </div>
        <div class="share-panel-actions">
          <div class="share-panel-search">
            <t-input v-model="searchQuery" size="small" :placeholder="$t('organization.share.searchPlaceholder')"
              clearable>
              <template #prefix-icon>
                <t-icon name="search" />
              </template>
            </t-input>
          </div>
          <t-popup v-model="addPopupVisible" trigger="click" placement="bottom-end" destroy-on-close
            overlay-class-name="share-add-popup-overlay">
            <t-button theme="primary" variant="outline" shape="square" size="small" class="share-panel-add-btn"
              :title="$t('knowledgeEditor.share.addShare')" :aria-label="$t('knowledgeEditor.share.addShare')">
              <template #icon><t-icon name="add" /></template>
            </t-button>
            <template #content>
              <div class="share-add-popup-inner" @click.stop>
                <div class="member-invite-popup-title">{{ $t('organization.share.addShareDialogTitle') }}</div>
                <div class="org-upgrade-fields">
                  <div class="org-upgrade-field org-upgrade-field--last">
                    <label class="org-upgrade-field-label">{{ $t('organization.share.selectOrg') }}</label>
                    <ShareToSpaceOrgSelect v-model="selectedOrgId" :organizations="availableOrganizations"
                      :loading="loadingOrgs" />
                  </div>
                </div>
                <!-- 有未保存的修改：共享的是已保存的配置，先确认；当前选择保持不变 -->
                <t-alert v-if="confirmingUnsaved" theme="warning" class="share-unsaved-alert"
                  :message="$t('organization.share.unsavedBeforeShare')" />
                <div class="invite-popup-footer">
                  <template v-if="confirmingUnsaved">
                    <t-button variant="outline" :disabled="submitting" @click="cancelShareWithUnsaved">
                      {{ $t('common.cancel') }}
                    </t-button>
                    <t-button theme="primary" :loading="submitting" @click="saveAndShare">
                      {{ $t('organization.share.saveAndShare') }}
                    </t-button>
                  </template>
                  <template v-else>
                    <t-button variant="outline" :disabled="submitting" @click="addPopupVisible = false">
                      {{ $t('common.cancel') }}
                    </t-button>
                    <t-button theme="primary" :loading="submitting" :disabled="!selectedOrgId" @click="handleShare">
                      {{ $t('knowledgeEditor.share.addShare') }}
                    </t-button>
                  </template>
                </div>
              </div>
            </template>
          </t-popup>
        </div>
      </div>

      <div v-if="loadingShares && shares.length === 0" class="share-panel-loading">
        <t-loading size="small" />
        <span>{{ $t('organization.share.loading') }}</span>
      </div>
      <div v-else-if="filteredShares.length === 0" class="share-panel-empty">
        <t-empty :description="searchQuery.trim()
          ? $t('organization.share.emptySearch', { q: searchQuery })
          : $t('organization.share.noShares')" />
      </div>
      <div v-else class="share-panel-table-shell">
        <t-table row-key="id" :data="filteredShares" :columns="shareColumns" size="medium" hover stripe
          :loading="loadingShares">
          <template #space="{ row }">
            <div class="share-space-cell">
              <span class="share-space-name">
                <SpaceAvatar :name="row.organization_name || ''"
                  :avatar="getOrgForShare(row.organization_id)?.avatar" size="small" />
                <span class="share-space-name-text">{{ row.organization_name }}</span>
              </span>
              <span v-if="row.shared_by_username" class="share-space-meta">
                {{ $t('organization.share.sharedFrom') }} {{ row.shared_by_username }}
              </span>
            </div>
          </template>
          <template #permission>
            <t-tag size="small" theme="default" variant="light">
              {{ $t('organization.share.permissionReadonly') }}
            </t-tag>
          </template>
          <template #created_at="{ row }">{{ formatShareDate(row.created_at) }}</template>
          <template #actions="{ row }">
            <div class="share-table-actions">
              <t-popconfirm :content="$t('knowledgeEditor.share.unshareConfirm', { name: row.organization_name })"
                :confirm-btn="{ content: $t('common.confirm'), theme: 'danger' }"
                :cancel-btn="{ content: $t('common.cancel') }" placement="left" @confirm="handleUnshare(row)">
                <t-tooltip :content="$t('organization.share.unshareAction')" placement="top">
                  <t-button theme="danger" shape="square" variant="text" size="small" @click.stop>
                    <template #icon><t-icon name="delete" /></template>
                  </t-button>
                </t-tooltip>
              </t-popconfirm>
            </div>
          </template>
        </t-table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import { useOrganizationStore } from '@/stores/organization'
import { listAgentShares } from '@/api/organization'
import type { AgentShareResponse, AgentReadinessIssue } from '@/api/organization'
import type { CustomAgent } from '@/api/agent'
import SpaceAvatar from '@/components/SpaceAvatar.vue'
import ShareToSpaceOrgSelect from '@/components/ShareToSpaceOrgSelect.vue'
import { formatLocalizedList } from '@/utils/format-list'
import {
  agentReadinessIssueLabels,
  hasReadinessHighlightField,
  shareRefusalIssues,
} from '@/utils/agent-share-readiness'

const { t, locale } = useI18n()
const orgStore = useOrganizationStore()

function getOrgForShare(organizationId: string) {
  return orgStore.organizations.find(o => o.id === organizationId)
}

interface Props {
  agentId: string
  agent?: CustomAgent | null
  /** 编辑窗口里是否有未保存的修改。共享接口读取的是已保存的配置，所以有修改时先确认。 */
  isDirty?: () => boolean
  /** 保存编辑窗口里的修改但不关闭窗口；返回是否保存成功。 */
  saveDraft?: () => Promise<boolean>
}

const props = defineProps<Props>()

const emit = defineEmits<{
  /** 共享被拒并带有缺项：由编辑窗口切到「模型配置」并定位到对应字段。 */
  (e: 'configure', issues: AgentReadinessIssue[]): void
}>()

// Members run this agent's skills in this workspace's sandbox, with the
// environment values its admins configured for them, and can have the model
// print those values. Sharing stays allowed; the owner is told before sharing.
const sharesSkillSecrets = computed(() => {
  const config = props.agent?.config
  if (!config?.sandbox_config_id) return false
  return config.skills_selection_mode === 'all'
    || (config.skills_selection_mode === 'selected' && (config.selected_skills?.length ?? 0) > 0)
})

const shareHintPopupInnerStyle = {
  boxSizing: 'border-box' as const,
  padding: '0',
  width: 'min(400px, calc(100vw - 24px))',
  maxWidth: 'min(400px, calc(100vw - 24px))',
  maxHeight: 'min(280px, 65vh)',
  overflow: 'hidden',
}

const loadingOrgs = ref(false)
const loadingShares = ref(false)
const submitting = ref(false)
const addPopupVisible = ref(false)
const searchQuery = ref('')
const selectedOrgId = ref('')
const shares = ref<(AgentShareResponse & { organization_name?: string })[]>([])
// 点「共享」时编辑窗口还有未保存的修改：弹层内改为确认（保存并共享 / 取消）。
// 记下是针对哪个共享空间做的确认，换了选择就自动回到普通的共享按钮。
const confirmingOrgId = ref<string | null>(null)
const confirmingUnsaved = computed(() =>
  confirmingOrgId.value !== null && confirmingOrgId.value === selectedOrgId.value
)
// 最近一次共享被拒时后端给出的缺项
const refusedIssues = ref<AgentReadinessIssue[]>([])

const refusedIssueLabels = computed(() => agentReadinessIssueLabels((key) => t(key), refusedIssues.value))
const refusedIssueSummary = computed(() => formatLocalizedList(refusedIssueLabels.value, locale.value))
// 只有缺项能对应到编辑窗口里的字段时才给"去设置"入口
const canGoConfigure = computed(() => hasReadinessHighlightField(refusedIssues.value))

const availableOrganizations = computed(() => {
  const sharedOrgIds = new Set(shares.value.map(s => s.organization_id))
  return orgStore.organizations.filter(
    (org) =>
      !sharedOrgIds.has(org.id) &&
      (org.is_owner === true || org.my_role === 'admin' || org.my_role === 'editor')
  )
})

const filteredShares = computed(() => {
  const query = searchQuery.value.trim().toLowerCase()
  if (!query) return shares.value
  return shares.value.filter((share) => {
    const haystack = [share.organization_name, share.shared_by_username].filter(Boolean).join(' ').toLowerCase()
    return haystack.includes(query)
  })
})

const shareColumns = computed(() => [
  { colKey: 'space', title: t('organization.share.columns.space'), ellipsis: true, minWidth: 180 },
  { colKey: 'permission', title: t('organization.share.columns.permission'), width: 96 },
  { colKey: 'created_at', title: t('organization.share.columns.sharedAt'), width: 154 },
  { colKey: 'actions', title: t('organization.share.columns.operations'), width: 72, align: 'left' },
])

function formatShareDate(dateStr?: string) {
  if (!dateStr) return '—'
  const date = new Date(dateStr)
  if (Number.isNaN(date.getTime())) return dateStr
  return date.toLocaleDateString(undefined, { year: 'numeric', month: '2-digit', day: '2-digit' })
}

async function loadOrganizations() {
  loadingOrgs.value = true
  try {
    await orgStore.fetchOrganizations()
  } finally {
    loadingOrgs.value = false
  }
}

async function loadShares() {
  if (!props.agentId) return
  loadingShares.value = true
  try {
    const result = await listAgentShares(props.agentId)
    if (result.success && result.data) {
      const sharesData = (result.data as { shares?: AgentShareResponse[] }).shares || result.data
      const sharesList = Array.isArray(sharesData) ? sharesData : []
      shares.value = sharesList.map((share: AgentShareResponse) => ({
        ...share,
        organization_name: share.organization_name || orgStore.organizations.find(o => o.id === share.organization_id)?.name || share.organization_id
      }))
    }
  } catch (e) {
    console.error('Failed to load agent shares:', e)
  } finally {
    loadingShares.value = false
  }
}

async function handleShare() {
  if (!selectedOrgId.value) return
  // 有未保存的修改：共享出去的是已保存的配置。先确认，选择保持不变，不发请求。
  if (props.isDirty?.()) {
    confirmingOrgId.value = selectedOrgId.value
    return
  }
  await shareSelected()
}

// 确认框里的「取消」：回到普通的共享按钮，选择保持不变
function cancelShareWithUnsaved() {
  confirmingOrgId.value = null
}

// 「保存并共享」：先保存，成功后再发共享请求；保存失败就停在这里。
async function saveAndShare() {
  if (!props.saveDraft) {
    confirmingOrgId.value = null
    return
  }
  submitting.value = true
  try {
    const saved = await props.saveDraft()
    confirmingOrgId.value = null
    if (!saved) {
      // 校验没过时编辑窗口会切到出错的页，这个弹层挂在页面顶层，不关会浮在别的页上。
      // 选择保留，重新打开弹层即可继续共享。
      addPopupVisible.value = false
      return
    }
    await shareSelected()
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : t('organization.share.shareFailed')
    MessagePlugin.error(message)
  } finally {
    submitting.value = false
  }
}

function goConfigure() {
  emit('configure', refusedIssues.value)
}

async function shareSelected() {
  if (!selectedOrgId.value) return
  submitting.value = true
  try {
    const result = await orgStore.shareAgent(props.agentId, {
      organization_id: selectedOrgId.value,
      permission: 'viewer'
    })
    if (result.success) {
      MessagePlugin.success(t('organization.share.shareSuccess'))
      selectedOrgId.value = ''
      addPopupVisible.value = false
      refusedIssues.value = []
      await loadShares()
      return
    }
    const issues = shareRefusalIssues(result)
    if (issues.length > 0) {
      // 后端说明了缺什么：按缺项提示并给出"去设置"，不再显示笼统的英文提示
      refusedIssues.value = issues
      addPopupVisible.value = false
      return
    }
    refusedIssues.value = []
    MessagePlugin.error(result.message || t('organization.share.shareFailed'))
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : t('organization.share.shareFailed')
    MessagePlugin.error(message)
  } finally {
    submitting.value = false
  }
}

async function handleUnshare(share: AgentShareResponse) {
  try {
    const result = await orgStore.unshareAgent(
      props.agentId,
      share.id,
      share.organization_id
    )
    if (result.success) {
      MessagePlugin.success(t('organization.share.unshareSuccess'))
      await loadShares()
    } else {
      MessagePlugin.error(result.message || t('organization.share.unshareFailed'))
    }
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : t('organization.share.unshareFailed')
    MessagePlugin.error(message)
  }
}

watch(() => props.agentId, async (newId) => {
  confirmingOrgId.value = null
  refusedIssues.value = []
  if (newId) await Promise.all([loadOrganizations(), loadShares()])
}, { immediate: true })

onMounted(async () => {
  if (props.agentId) await Promise.all([loadOrganizations(), loadShares()])
})

defineExpose({ loadShares })
</script>

<style scoped lang="less">
@import '@/components/share-to-space-panel.less';

.share-unsaved-alert {
  margin-bottom: 8px;
}
</style>

<style lang="less">
@import '@/components/share-to-space-panel.overlay.less';
</style>
