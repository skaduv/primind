import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

// Enforce the squash-merge title, not each intermediate/WIP commit.
export function validTitle(title) {
  return typeof title === 'string' &&
    /^(?:feat|fix|docs|refactor|test|chore|ci|build|perf|revert)(?:\([a-zA-Z0-9_.\/-]+\))?!?: \S[^\r\n]*$/u.test(title) &&
    title === title.trim();
}

export function checkEvent(event) {
  if (!event || typeof event !== 'object' || !event.pull_request) {
    throw new Error('A pull_request event is required for PR policy checks');
  }
  if (!validTitle(event.pull_request.title)) {
    throw new Error('PR title must use type(scope): description, for example fix(auth): 修复共享撤销后仍可访问. Types: feat, fix, docs, refactor, test, chore, ci, build, perf, revert. Scope is optional.');
  }
}

export function main(env = process.env) {
  if (!env.GITHUB_EVENT_PATH) throw new Error('GITHUB_EVENT_PATH is required');
  // Parse data instead of interpolating a PR title into an executable command.
  checkEvent(JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8')));
  console.log('PR title policy passed');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); } catch (error) {
    console.error(`PR policy failed: ${error.message}`);
    process.exitCode = 1;
  }
}
