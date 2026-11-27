import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('./AgentEditorModal.vue', import.meta.url), 'utf8')

test('editing an agent closes the editor after a successful save', () => {
  assert.match(
    source,
    /await updateAgent\(formData\.value\.id, payload\);\s*MessagePlugin\.success\(t\('agent\.messages\.updated'\)\);\s*emit\('success'\);\s*handleClose\(\);/
  )
})

test('the first successful create stays open for integration setup', () => {
  const createBranch = source.match(
    /if \(editorMode\.value === 'create'\) \{([\s\S]*?)^\s{4}\} else \{/m
  )?.[1]

  assert.ok(createBranch, 'expected to find the create branch')
  assert.doesNotMatch(createBranch, /handleClose\(\)/)
  assert.match(createBranch, /savedAgent\.value = created;/)
})

test('save button labels distinguish create from save-and-close', () => {
  assert.match(
    source,
    /const saveButtonLabel = computed\(\(\) =>\s*editorMode\.value === 'create'\s*\? t\('agent\.editor\.buttons\.create'\)\s*: t\('agent\.editor\.buttons\.saveAndClose'\)\s*\)/
  )
  assert.match(source, /saveButtonLabel/)
})

test('shows a post-create hint after the first successful save', () => {
  assert.match(source, /const isPostCreateSession = computed\(\(\) => !!savedAgent\.value\)/)
  assert.match(source, /settings-footer-note/)
  assert.match(source, /agent\.editor\.postCreateHint\.title/)
})

// Locate a settings row by the i18n key of its label and return the attributes
// that sit before class="setting-row" — i.e. whatever v-if guards that row.
// Anchoring on the label keeps these assertions stable across reformatting.
function settingRowGuard(labelKey) {
  const key = labelKey.replace(/\./g, '\\.')
  const row = source.match(
    new RegExp(
      `<div([^>]*)class="setting-row">\\s*<div class="setting-info">\\s*<label>\\{\\{ \\$t\\('${key}'\\) \\}\\}`
    )
  )
  assert.ok(row, `expected to find the ${labelKey} row`)
  return row[1]
}

test('conversation settings stay reachable in smart-reasoning mode', () => {
  // Agent mode keeps the section: it explains that history follows the context
  // window and hosts retain_retrieval_history, so it must not be gated on mode.
  assert.match(source, /v-show="currentSection === 'conversation'"/)
  assert.doesNotMatch(source, /currentSection === 'conversation' && !isAgentMode/)

  // The nav entry is pushed unconditionally; a re-introduced guard would indent it.
  const navItems = source.match(/const navItems = computed\(\(\) => \{([\s\S]*?)^\}\);/m)?.[1]
  assert.ok(navItems, 'expected to find the nav items computed')
  assert.match(navItems, /^  items\.push\(\{ key: 'conversation'/m)

  // Switching to agent mode must not evict the user from the section.
  // Tolerates the watch being dropped entirely by a later cleanup.
  const modeWatch = source.match(/watch\(isAgentMode, \(isAgent\) => \{([\s\S]*?)^\}\);/m)?.[1] ?? ''
  assert.doesNotMatch(modeWatch, /conversation/)
})

test('history turns stays hidden in smart-reasoning mode', () => {
  // The agent path sizes history by the context window and never reads
  // history_turns (session_agent_qa.go -> LoadAgentHistory), so an input there
  // would change nothing. KnowledgeQA still caps its history by it.
  const guard = settingRowGuard('agent.editor.historyTurns')
  assert.match(guard, /v-if="!isAgentMode && formData\.config\.multi_turn_enabled"/)
})

test('multi-turn switch stays hidden in smart-reasoning mode', () => {
  // CustomAgent.EnsureDefaults pins MultiTurnEnabled to true for smart-reasoning,
  // so an editable switch would silently revert after save.
  const guard = settingRowGuard('agent.editor.multiTurn')
  assert.match(guard, /v-if="!isAgentMode"/)
})

test('query rewrite stays hidden in smart-reasoning mode', () => {
  // enable_rewrite is consumed by the KnowledgeQA pipeline only.
  const guard = settingRowGuard('agent.editor.enableRewrite')
  assert.match(guard, /!isAgentMode/)
})

test('the section description matches what the mode actually shows', () => {
  // The default copy mentions query rewriting, which agent mode does not show.
  assert.match(source, /isAgentMode\.value\s*\?\s*t\('agentEditor\.desc\.conversationSectionAgent'\)/)
  assert.match(source, /<p class="section-description">\{\{ conversationSectionDesc \}\}<\/p>/)
})

test('history turns can be raised beyond the old 20 cap', () => {
  // KnowledgeQA history is capped by turns alone; 20 was too tight for the
  // windows current models have.
  const input = source.match(/<t-input-number v-model="formData\.config\.history_turns"[^>]*>/)
  assert.ok(input, 'expected to find the history turns input')
  assert.match(input[0], /:max="100"/)
})

test('retrieval retention is offered to agents that actually have a knowledge base', () => {
  // retain_retrieval_history is read only by the agent path
  // (internal/agent/observe.go) and only rewrites KB/Wiki tool results, so it
  // is meaningless without a knowledge base.
  const guard = settingRowGuard('agent.editor.retainRetrievalHistory')
  assert.match(guard, /isAgentMode && hasKnowledgeBase/)
})

test('skills and sandbox share one editor section', () => {
  const navItems = source.match(/const navItems = computed\(\(\) => \{([\s\S]*?)^\}\);/m)?.[1]
  assert.ok(navItems, 'expected to find the nav items computed')
  assert.match(navItems, /key: 'skills'/)
  assert.match(navItems, /icon: SKILL_ICON/)
  assert.doesNotMatch(navItems, /key: 'sandbox'/)

  const capabilityGroup = source.match(/pickItems\(\['multimodal', 'tools', 'mcp', 'skills'\]\)/)
  assert.ok(capabilityGroup, 'expected the capability group to list skills without a separate sandbox tab')

  assert.match(source, /v-show="currentSection === 'skills' && isAgentMode"/)
  assert.doesNotMatch(source, /currentSection === 'sandbox' && isAgentMode/)
  assert.match(source, /sandbox: 'skills'/)
  assert.match(source, /formData\.config\.sandbox_config_id/)
  assert.match(source, /:disabled="!canEnableSkills"/)
  assert.match(source, /sandbox-option/)
  assert.doesNotMatch(source, /skill-info-box/)
})

test('agent skill picker uses the catalog and only enables ready installs', () => {
  assert.match(source, /function autoBindSoleSandbox\(/)
  assert.match(source, /canEnableSkills/)
  assert.match(source, /catalogSkillRows/)
  assert.match(source, /showCatalogSkillList/)
  assert.match(source, /skillsSelectionMode\.value !== 'none'/)
  assert.match(source, /:disabled="!skill\.selectable"/)
  assert.match(source, /catalogSkillGroups/)
  assert.match(source, /installPartial/)
  assert.match(source, /installCatalogToCurrent/)
  assert.match(source, /agent\.editor\.installToThisSandbox/)
  assert.match(source, /skill-pick-list/)
  assert.match(source, /skill-pick-group/)
  assert.match(source, /skill-pick__badge/)
  assert.match(source, /skillStatusIcon/)
  assert.match(source, /isSkillBusy/)
  assert.match(source, /viewInstallProgress/)
  assert.match(source, /openSkillInstallProgress/)
  assert.doesNotMatch(source, /await openSkillInstallProgress\(skill\)/)
  assert.match(source, /SandboxSkillsPanel/)
  assert.match(source, /focus-skill-id/)
  assert.match(source, /skillsGroupUnavailable/)
  assert.match(source, /selectedSandboxSummary/)
  assert.match(source, /line-clamp: 2/)
  assert.doesNotMatch(source, /skillsSelectionMode === 'selected' && catalogSkillRows/)
  assert.doesNotMatch(source, /skill-list-summary/)
  assert.doesNotMatch(source, /skill-ready-stat/)
})

test('agent skill picker offers upgrades to admins only and keeps upgrading skills selected', () => {
  assert.match(source, /from '@\/utils\/skillUpgrade'/)
  assert.match(source, /const upgradable = Boolean\(inst && installUpgradable\(item, inst\)\)/)
  assert.match(source, /canInstallSkills\.value && hasSandboxSelected\.value && skill\.upgradable/)
  assert.match(source, /v-if="canUpgradeSkillRow\(skill\)"/)
  assert.match(source, /v-else-if="canUpgradeSkillRow\(skill\)"/)
  assert.match(source, /agent\.editor\.upgradeOnThisSandbox/)
  // The progress drawer reads admin-only endpoints, so it is not offered to others.
  assert.match(source, /v-else-if="canInstallSkills && isSkillBusy\(skill\)"/)
  assert.match(source, /skill\.selectable \|\| \(skill\.installed && isSkillBusy\(skill\)\)/)
})

test('a skill whose previous version still runs stays selectable during and after an upgrade', () => {
  assert.match(source, /installEnabled && \(installStatus === 'ready' \|\| Boolean\(servedNote\)\)/)
  assert.match(source, /v-if="skill\.selectable && skill\.servedNote"/)
  // Installing the catalog version over a failed upgrade is the upgrade retried.
  assert.match(source, /installsAnUpgrade\(skill\) \? \$t\('settings\.skills\.upgrade'\) : \$t\('agent\.editor\.installShort'\)/)
  assert.match(source, /return skill\.upgradable \|\| Boolean\(skill\.servedNote\)/)
})

test('retrieval strategy settings are hidden in agent mode, where search_knowledge ignores them', () => {
  // search_knowledge reads the global retrieval configuration; showing the
  // per-agent thresholds in agent mode let users tune values with no effect.
  assert.match(source, /v-show="currentSection === 'retrieval' && hasKnowledgeBase && !isAgentMode"/)
  assert.match(source, /if \(hasKnowledgeBase\.value && !isAgentMode\.value\) \{\s*items\.push\(\{ key: 'retrieval'/)
  const modeWatch = source.match(/watch\(isAgentMode, \(isAgent\) => \{([\s\S]*?)^\}\);/m)?.[1] ?? ''
  assert.match(modeWatch, /isAgent && \([^)]*currentSection\.value === 'retrieval'/)
})

// Sharing reads the saved configuration, so the share panel needs to know
// whether the editor holds unsaved edits, and needs a save that keeps the
// window open ("保存并共享").
test('the share panel is told about unsaved edits and can save without closing', () => {
  assert.match(
    source,
    /<AgentShareSettings :agent-id="editorAgent\.id" :agent="editorAgent" :is-dirty="\(\) => modalShell\.isDirty\(\)"\s*:save-draft="saveDraft" @configure="handleShareConfigure" \/>/,
  )
  assert.match(source, /const handleSave = \(\) => saveAgentForm\(true\);/)
  assert.match(source, /const saveDraft = \(\) => saveAgentForm\(false\);/)
})

test('saving before a share keeps the editor open and marks the form saved', () => {
  const branch = source.match(
    /const saved: any = await updateAgent\(formData\.value\.id, payload\);([\s\S]*?)return true;/,
  )?.[1]
  assert.ok(branch, 'expected the save-without-close branch')
  assert.doesNotMatch(branch, /handleClose\(\)/)
  assert.match(branch, /emit\('success'\);/)
  assert.match(branch, /modalShell\.markClean\(\);/)
  assert.match(branch, /notifyAgentSaveWarnings\(saved\?\.warnings\);/)
  // The regular save still warns about an already shared agent.
  assert.match(source, /handleClose\(\);\s*notifyAllowedToolsWarnings\(updated\?\.allowed_tools_warnings\);\s*notifyAgentSaveWarnings\(updated\?\.warnings\);/)
})

test('a refused share sends the editor to the model section and the missing field', () => {
  assert.match(
    source,
    /const handleShareConfigure = \(issues: AgentReadinessIssue\[\]\) => \{\s*const field = firstReadinessHighlightField\(issues\);\s*if \(field\) applyInitialFieldHighlight\(field\);\s*\};/,
  )
})

// Saves that keep the editor open (after create, "保存并共享") must take the
// tool list the server stored: it may rename legacy tools or drop tools other
// settings control. Otherwise the tools page keeps the old ticks while the
// form counts as saved, and the next save resubmits them.
test('saves that keep the editor open adopt the tool list the server stored', async () => {
  const start = source.indexOf('const syncSavedAllowedTools =')
  const end = source.indexOf('// 校验并保存当前表单', start)
  assert.ok(start >= 0 && end > start, 'expected syncSavedAllowedTools')
  const ts = (await import('typescript')).default
  const vm = await import('node:vm')
  const { ref } = await import('vue')
  const formData = ref({ config: { allowed_tools: ['knowledge_search', 'dropped_tool'] } })
  const storedToolNames = ref(['knowledge_search', 'dropped_tool'])
  const keepAllowedToolsUnset = ref(true)
  const { syncSavedAllowedTools } = vm.runInNewContext(
    ts.transpile(`${source.slice(start, end)}\n({ syncSavedAllowedTools })`),
    {
      formData, storedToolNames, keepAllowedToolsUnset,
      normalizeLegacyToolNames: (names) => names.map((name) => (name === 'knowledge_search' ? 'search_knowledge' : name)),
    },
  )

  // Arrays built inside the sandbox have its prototype; compare plain copies.
  const tools = () => Array.from(formData.value.config.allowed_tools)
  syncSavedAllowedTools({ config: { allowed_tools: ['knowledge_search'] } })
  assert.deepEqual(tools(), ['search_knowledge'])
  assert.deepEqual(Array.from(storedToolNames.value), ['search_knowledge'])
  assert.equal(keepAllowedToolsUnset.value, false)

  // A response without a tool list leaves the form alone.
  syncSavedAllowedTools({ config: {} })
  syncSavedAllowedTools(undefined)
  assert.deepEqual(tools(), ['search_knowledge'])

  // Both branches sync before the form is marked saved.
  const createBranch = source.match(/savedAgent\.value = created;([\s\S]*?)modalShell\.markClean\(\);/)?.[1] ?? ''
  assert.match(createBranch, /syncSavedAllowedTools\(created\);/)
  const draftBranch = source.match(
    /const saved: any = await updateAgent\(formData\.value\.id, payload\);([\s\S]*?)modalShell\.markClean\(\);/,
  )?.[1] ?? ''
  assert.match(draftBranch, /syncSavedAllowedTools\(saved\?\.data\);/)
})
