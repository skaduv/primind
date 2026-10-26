import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, renameSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { COMPONENTS, changedPaths, githubOutputs, selectChecks } from './changes.mjs';
import { checkEvent, validTitle } from './policy.mjs';
import { evaluateGate } from './gate.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));

function temporaryDirectory(t) {
  const root = resolve(tmpdir());
  const directory = mkdtempSync(join(root, 'primind-ci-test-'));
  t.after(() => {
    // Only remove the unique directory created by this test, never the repo.
    assert.ok(resolve(directory).startsWith(root + sep));
    rmSync(directory, { recursive: true, force: true });
  });
  return directory;
}

function runScript(name, env, cwd = process.cwd()) {
  return spawnSync(process.execPath, [join(scriptDir, name)], {
    cwd, env: { ...process.env, ...env }, encoding: 'utf8',
  });
}

function selected(paths) {
  return Object.entries(selectChecks(paths).scopes).filter(([, value]) => value).map(([key]) => key);
}

test('documentation-only changes need no component checks', () => {
  assert.deepEqual(selected(['README.md', 'CONTRIBUTING.md', 'docs/CI_GATE_SETUP.md', '.github/pull_request_template.md']), []);
});

test('runtime Markdown and unknown source/config paths fail toward app coverage', () => {
  for (const path of ['internal/prompts/prompt.md', 'config/models.yaml', 'new-feature/schema.json', 'docs/swagger.json']) {
    assert.deepEqual(selected([path]), ['app'], path);
  }
});

test('client Go changes also verify dependent CLI and both lint modules', () => {
  assert.deepEqual(selected(['client/client.go']), ['cli', 'client', 'lint']);
  assert.deepEqual(selectChecks(['client/client.go']).lintModules, ['cli', 'client']);
});

test('CLI Go changes remain scoped to CLI', () => {
  assert.deepEqual(selected(['cli/internal/run.go']), ['cli', 'lint']);
  assert.deepEqual(selectChecks(['cli/internal/run.go']).lintModules, ['cli']);
});

test('root dependency changes exercise root consumers', () => {
  for (const path of ['go.mod', 'go.sum']) {
    assert.deepEqual(selected([path]), ['app', 'docreader', 'anydoc', 'lint']);
    assert.deepEqual(selectChecks([path]).lintModules, ['.']);
  }
});

test('central configuration changes exercise every routed component', () => {
  for (const path of ['.github/workflows/ci.yml', '.github/workflows/app.yml', 'scripts/ci/changes.mjs', '.golangci.yml']) {
    assert.deepEqual(selected([path]), [...COMPONENTS]);
    assert.deepEqual(selectChecks([path]).lintModules, ['.', 'cli', 'client']);
  }
});

test('component-specific changes preserve existing workflow inputs', () => {
  const examples = [
    ['frontend/src/view.vue', ['frontend']],
    ['scripts/build_frontend_dist.sh', ['frontend']],
    ['scripts/test_embed_nginx.py', ['frontend']],
    ['docreader/parser.py', ['docreader']],
    ['docreader/client/client.go', ['docreader', 'lint']],
    ['docker/Dockerfile.docreader', ['docreader']],
    ['docker/Dockerfile.odl-hybrid', ['docreader']],
    ['helm/values.yaml', ['helm']],
    ['third_party/anydoc-go/src/lib.rs', ['app', 'anydoc']],
    ['internal/infrastructure/docparser/parser.go', ['app', 'anydoc', 'lint']],
    ['scripts/build-anydoc-lib.sh', ['app', 'anydoc']],
    ['testdata/sample.txt', ['app', 'docreader']],
    ['THIRD_PARTY_NOTICES.md', ['app']],
    ['internal/service.go', ['app', 'lint']],
  ];
  for (const [path, expected] of examples) assert.deepEqual(selected([path]), expected, path);
});

test('path selection handles more than 300 changes and unusual names', () => {
  const paths = Array.from({ length: 400 }, (_, i) => `docs/note-${i}.md`);
  paths.push('client/带 空格.go', 'frontend/a\nb.vue');
  assert.deepEqual(selected(paths), ['frontend', 'cli', 'client', 'lint']);
});

test('Git diff includes deletions and both sides of a cross-module rename', (t) => {
  const directory = temporaryDirectory(t);
  const git = (...args) => execFileSync('git', args, { cwd: directory, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  git('init', '-q');
  git('config', 'user.name', 'CI fixture');
  git('config', 'user.email', 'ci-fixture@example.invalid');
  git('config', 'commit.gpgsign', 'false');
  mkdirSync(join(directory, 'internal'));
  mkdirSync(join(directory, 'frontend'));
  mkdirSync(join(directory, 'client'));
  writeFileSync(join(directory, 'internal', '原 文件.go'), 'package old\n');
  writeFileSync(join(directory, 'frontend', 'deleted.vue'), '<template />\n');
  git('add', '--all');
  git('commit', '-qm', 'fixture baseline');
  const base = git('rev-parse', 'HEAD');
  renameSync(join(directory, 'internal', '原 文件.go'), join(directory, 'client', '新 文件.go'));
  rmSync(join(directory, 'frontend', 'deleted.vue'));
  git('add', '--all');
  git('commit', '-qm', 'fixture changes');
  const head = git('rev-parse', 'HEAD');
  const paths = changedPaths(base, head, directory);
  assert.deepEqual([...paths].sort(), ['client/新 文件.go', 'frontend/deleted.vue', 'internal/原 文件.go'].sort());
  assert.deepEqual(selected(paths), ['app', 'frontend', 'cli', 'client', 'lint']);
  const output = join(directory, 'github-output');
  const run = runScript('changes.mjs', { BASE_SHA: base, HEAD_SHA: head, GITHUB_OUTPUT: output }, directory);
  assert.equal(run.status, 0, run.stderr);
  assert.match(readFileSync(output, 'utf8'), /^lint_modules=\["\.","cli","client"\]$/m);
  assert.throws(() => changedPaths('0'.repeat(40), head, directory));
});

test('change detection rejects missing or option-shaped revisions and Git errors', (t) => {
  const directory = temporaryDirectory(t);
  for (const value of [undefined, '', 'HEAD', '--help', 'abc; touch leaked']) {
    assert.throws(() => changedPaths(value, 'a'.repeat(40), directory), /BASE_SHA/);
  }
  const run = runScript('changes.mjs', { BASE_SHA: 'a'.repeat(40), HEAD_SHA: 'b'.repeat(40) }, directory);
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /Change detection failed/);
});

test('outputs contain strict booleans and a complete machine-readable scope map', () => {
  const selection = selectChecks(['frontend/src/app.vue']);
  const output = githubOutputs(selection);
  for (const name of COMPONENTS) assert.match(output, new RegExp(`^${name}=(true|false)$`, 'm'));
  const encoded = output.split('\n').find((line) => line.startsWith('scopes=')).slice(7);
  assert.deepEqual(JSON.parse(encoded), selection.scopes);
});

test('PR title supports Chinese, optional scope and breaking changes', () => {
  for (const title of ['feat: 新增资料检索', 'fix(auth): 修复撤权', 'refactor(api)!: 调整契约', 'ci: add gate', 'revert: 撤销接口变更']) {
    assert.equal(validTitle(title), true, title);
  }
  for (const title of ['', '修复问题', 'feat: ', 'feat : x', 'feat: x\nci: y', 'feat(auth): x ', ' feat: x', 'feature: x', 'fix(): x', null]) {
    assert.equal(validTitle(title), false, String(title));
  }
});

test('PR title policy checks current event data, including edited titles', () => {
  assert.throws(() => checkEvent({ action: 'opened', pull_request: { title: 'WIP' } }), /PR title/);
  assert.doesNotThrow(() => checkEvent({ action: 'edited', pull_request: { title: 'fix: 修正标题' } }));
  assert.throws(() => checkEvent({}), /pull_request event/);
});

test('PR title is treated as data and no body checkboxes are required', (t) => {
  const directory = temporaryDirectory(t);
  const eventPath = join(directory, 'event.json');
  writeFileSync(eventPath, JSON.stringify({ pull_request: { title: 'fix: $(touch injected) `touch injected`', body: '' } }));
  const run = runScript('policy.mjs', { GITHUB_EVENT_PATH: eventPath }, directory);
  assert.equal(run.status, 0, run.stderr);
  assert.equal(existsSync(join(directory, 'injected')), false);
  writeFileSync(eventPath, '{broken json');
  assert.notEqual(runScript('policy.mjs', { GITHUB_EVENT_PATH: eventPath }, directory).status, 0);
});

function needsFor(required = []) {
  const scopes = Object.fromEntries(COMPONENTS.map((name) => [name, required.includes(name)]));
  return {
    prepare: { result: 'success', outputs: { scopes: JSON.stringify(scopes) } },
    ...Object.fromEntries(COMPONENTS.map((name) => [name, { result: scopes[name] ? 'success' : 'skipped' }])),
  };
}

test('documentation-only gate passes with explicit unneeded skips', () => {
  assert.equal(evaluateGate(needsFor()).passed, true);
});

test('gate passes only when all selected components succeed', () => {
  assert.equal(evaluateGate(needsFor([...COMPONENTS])).passed, true);
  for (const name of COMPONENTS) {
    for (const result of ['failure', 'cancelled', 'skipped', 'neutral', 'pending', undefined]) {
      const needs = needsFor([name]);
      needs[name].result = result;
      assert.equal(evaluateGate(needs).passed, false, `${name}: ${result}`);
    }
  }
});

test('unselected components may succeed or skip but cannot fail or disappear', () => {
  for (const name of COMPONENTS) {
    const needs = needsFor();
    needs[name].result = 'success';
    assert.equal(evaluateGate(needs).passed, true);
    for (const result of ['failure', 'cancelled', 'unknown', undefined]) {
      needs[name].result = result;
      assert.equal(evaluateGate(needs).passed, false, `${name}: ${result}`);
    }
    delete needs[name];
    assert.equal(evaluateGate(needs).passed, false);
  }
});

test('preparation failures, malformed outputs and unknown scope keys fail closed', () => {
  for (const result of ['failure', 'cancelled', 'skipped', undefined]) {
    const needs = needsFor();
    needs.prepare.result = result;
    assert.equal(evaluateGate(needs).passed, false);
  }
  for (const encoded of [undefined, '', '{}', '[]', 'null', '{oops', JSON.stringify({ ...selectChecks([]).scopes, app: 'false' }), JSON.stringify({ ...selectChecks([]).scopes, other: false })]) {
    const needs = needsFor();
    needs.prepare.outputs.scopes = encoded;
    assert.equal(evaluateGate(needs).passed, false, String(encoded));
  }
  const needs = needsFor();
  needs.unexpected = { result: 'success' };
  assert.equal(evaluateGate(needs).passed, false);
  assert.equal(evaluateGate(null).passed, false);
});

test('gate process exposes failure through exit status and writes a summary', (t) => {
  const directory = temporaryDirectory(t);
  const summaryFile = join(directory, 'summary.md');
  const needs = needsFor(['app']);
  needs.app.result = 'skipped';
  const run = runScript('gate.mjs', { NEEDS_JSON: JSON.stringify(needs), GITHUB_STEP_SUMMARY: summaryFile }, directory);
  assert.notEqual(run.status, 0);
  assert.match(readFileSync(summaryFile, 'utf8'), /CI Gate: FAIL/);
  assert.notEqual(runScript('gate.mjs', { NEEDS_JSON: '{invalid' }, directory).status, 0);
  assert.equal(runScript('gate.mjs', { NEEDS_JSON: JSON.stringify(needsFor()) }, directory).status, 0);
});
