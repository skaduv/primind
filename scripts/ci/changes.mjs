import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

export const COMPONENTS = Object.freeze([
  'app', 'frontend', 'docreader', 'anydoc', 'cli', 'client', 'helm', 'lint',
]);

export function selectChecks(paths) {
  const scopes = Object.fromEntries(COMPONENTS.map((name) => [name, false]));
  const modules = new Set();
  const enable = (...names) => names.forEach((name) => { scopes[name] = true; });

  for (const path of paths) {
    // Changes to the gate itself must exercise every component it controls.
    if (path.startsWith('.github/workflows/') || path.startsWith('scripts/ci/') || path === '.golangci.yml') {
      enable(...COMPONENTS);
      ['.', 'cli', 'client'].forEach((name) => modules.add(name));
      continue;
    }

    if (path === 'go.mod' || path === 'go.sum') {
      enable('app', 'docreader', 'anydoc');
      modules.add('.');
      continue;
    }

    // The CLI uses replace github.com/primind/primind/client => ../client.
    if (path.startsWith('client/')) {
      enable('client', 'cli');
      if (/\.go$|\/(?:go\.mod|go\.sum)$/.test(path)) {
        modules.add('client');
        modules.add('cli');
      }
      continue;
    }
    if (path.startsWith('cli/')) {
      enable('cli');
      if (/\.go$|\/(?:go\.mod|go\.sum)$/.test(path)) modules.add('cli');
      continue;
    }
    if (path.startsWith('frontend/') || ['scripts/build_frontend_dist.sh', 'scripts/test_embed_nginx.py'].includes(path)) {
      enable('frontend');
      continue;
    }
    if (path.startsWith('docreader/') || ['docker/Dockerfile.docreader', 'docker/Dockerfile.odl-hybrid'].includes(path)) {
      enable('docreader');
      if (path.endsWith('.go')) modules.add('.');
      continue;
    }
    if (path.startsWith('helm/')) {
      enable('helm');
      continue;
    }
    if (path.startsWith('third_party/anydoc-go/') || path.startsWith('internal/infrastructure/docparser/') || path === 'scripts/build-anydoc-lib.sh') {
      enable('app', 'anydoc');
      if (path.endsWith('.go') || /\/(?:go\.mod|go\.sum)$/.test(path)) modules.add('.');
      continue;
    }
    if (path.startsWith('testdata/')) {
      enable('app', 'docreader');
      if (path.endsWith('.go')) modules.add('.');
      continue;
    }

    // Only explicitly documentary paths are cheap. Markdown under internal/
    // may be embedded into the application and must still trigger app checks.
    const documentation =
      (path.startsWith('docs/') && /\.(?:md|mdx|txt|png|jpe?g|svg|gif|webp|pdf)$/i.test(path)) ||
      (!path.includes('/') && /\.md$/i.test(path) && path !== 'THIRD_PARTY_NOTICES.md') ||
      /^\.github\/(?:pull_request_template\.md|PULL_REQUEST_TEMPLATE\/.*\.md)$/i.test(path) ||
      /^(?:LICENSE|NOTICE)(?:\.[^/]+)?$/.test(path);
    if (documentation) continue;

    // Conservative fallback: new runtime inputs and unknown source/config
    // paths must not silently evade CI. Add a focused route when appropriate.
    enable('app');
    if (path.endsWith('.go') || /\/(?:go\.mod|go\.sum)$/.test(path)) modules.add('.');
  }

  scopes.lint = modules.size > 0;
  return { scopes, lintModules: ['.', 'cli', 'client'].filter((name) => modules.has(name)) };
}

export function changedPaths(base, head, cwd = process.cwd()) {
  for (const [name, value] of [['BASE_SHA', base], ['HEAD_SHA', head]]) {
    if (typeof value !== 'string' || !/^(?:[a-f\d]{40}|[a-f\d]{64})$/i.test(value)) {
      throw new Error(`${name} must be a full Git commit SHA`);
    }
  }
  // Disabling rename detection emits both the removed and added path. NUL
  // delimiting preserves whitespace, newlines and non-ASCII file names.
  const result = execFileSync('git', ['diff', '--name-only', '--no-renames', '-z', `${base}...${head}`, '--'], {
    cwd, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return result.split('\0').filter(Boolean);
}

export function githubOutputs(selection) {
  return [
    ...COMPONENTS.map((name) => `${name}=${selection.scopes[name]}`),
    `scopes=${JSON.stringify(selection.scopes)}`,
    `lint_modules=${JSON.stringify(selection.lintModules)}`,
    '',
  ].join('\n');
}

export function main(env = process.env) {
  const paths = changedPaths(env.BASE_SHA, env.HEAD_SHA);
  const selection = selectChecks(paths);
  const output = githubOutputs(selection);
  if (env.GITHUB_OUTPUT) appendFileSync(env.GITHUB_OUTPUT, output, 'utf8');
  console.log(`Changed paths: ${paths.length}`);
  console.log(output.trimEnd());
  return selection;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); } catch (error) {
    console.error(`Change detection failed: ${error.message}`);
    process.exitCode = 1;
  }
}
