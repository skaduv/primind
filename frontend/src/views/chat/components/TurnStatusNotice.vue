<template>
    <div class="turn-status-notice" :class="[`is-${status}`, { 'is-embedded': embeddedMode }]" role="status"
        :data-turn-status="status">
        <t-icon name="error-circle" class="turn-status-notice__icon" />
        <span class="turn-status-notice__text">
            <strong class="turn-status-notice__title">{{ t(`chat.turnStatus.${status}`) }}</strong>
            <span v-if="detail" class="turn-status-notice__detail">{{ detail }}</span>
        </span>
        <t-button size="small" variant="outline" shape="round" class="turn-status-notice__retry" :disabled="disabled"
            :title="t('chat.turnStatus.retryTitle')" @click="emit('retry')">
            <template #icon><t-icon name="refresh" /></template>
            {{ t('chat.turnStatus.retry') }}
        </t-button>
    </div>
</template>
<script setup>
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { effectiveTurnStatus, turnErrorCode, turnErrorMessage } from '@/utils/messageTurnStatus';

// Explains why an assistant turn has no usable answer (failed / interrupted)
// and offers to ask the same question again (BF-03 §4.7).
const props = defineProps({
    session: { type: Object, required: true },
    disabled: { type: Boolean, default: false },
    embeddedMode: { type: Boolean, default: false },
});
const emit = defineEmits(['retry']);
const { t, te } = useI18n();

const status = computed(() => effectiveTurnStatus(props.session));
// The reason in the user's language when the backend's error_code is known;
// otherwise the explanation the backend stored with the message.
const detail = computed(() => {
    const code = turnErrorCode(props.session);
    const key = `chat.turnError.${code}`;
    if (code && te(key)) return t(key);
    return turnErrorMessage(props.session);
});
</script>
<style scoped>
.turn-status-notice {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 4px;
    padding: 6px 10px;
    border-radius: var(--app-radius-sm);
    font-size: var(--app-text-md);
    line-height: 1.5;
    color: var(--td-text-color-secondary);
    background: var(--td-bg-color-container-hover);
}

.turn-status-notice__icon {
    flex: none;
    font-size: var(--app-text-xl);
    color: var(--td-warning-color);
}

.turn-status-notice.is-failed .turn-status-notice__icon {
    color: var(--td-error-color);
}

.turn-status-notice__text {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
}

.turn-status-notice__title {
    color: var(--td-text-color-primary);
    font-weight: 500;
}

.turn-status-notice__detail {
    color: var(--td-text-color-placeholder);
    word-break: break-word;
}

.turn-status-notice__retry {
    flex: none;
}

.turn-status-notice.is-embedded {
    background: transparent;
    padding-left: 0;
    padding-right: 0;
}
</style>
