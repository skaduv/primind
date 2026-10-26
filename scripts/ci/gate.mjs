import { appendFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { COMPONENTS } from './changes.mjs';

const isRecord = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
const sameKeys = (value, keys) => isRecord(value) && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));

export function evaluateGate(needs) {
  const errors = [];
  const rows = [];
  if (!sameKeys(needs, ['prepare', ...COMPONENTS])) {
    return { passed: false, errors: ['Missing or unexpected jobs in NEEDS_JSON'], rows };
  }
  if (needs.prepare?.result !== 'success') errors.push('prepare must complete successfully');
  let scopes;
  try {
    const encoded = needs.prepare?.outputs?.scopes;
    if (typeof encoded !== 'string') throw new Error('missing scopes output');
    scopes = JSON.parse(encoded);
  } catch {
    errors.push('prepare.outputs.scopes must be valid JSON');
  }
  if (!sameKeys(scopes, COMPONENTS) || COMPONENTS.some((name) => typeof scopes[name] !== 'boolean')) {
    errors.push('scopes must contain exactly one boolean for every CI component');
    return { passed: false, errors, rows };
  }
  for (const name of COMPONENTS) {
    const result = needs[name]?.result;
    const required = scopes[name];
    const accepted = result === 'success' || (!required && result === 'skipped');
    rows.push({ name, required, result: typeof result === 'string' ? result : 'missing', accepted });
    if (!accepted) errors.push(`${name}: ${required ? 'required check' : 'unselected check'} returned ${JSON.stringify(result) ?? 'missing'}`);
  }
  return { passed: errors.length === 0, errors, rows };
}

export function summary(report) {
  const safeCell = (value) => String(value).replace(/[\r\n|]/g, ' ');
  return [
    `## CI Gate: ${report.passed ? 'PASS' : 'FAIL'}`,
    '',
    '| Component | Required for this change | Result |',
    '| --- | --- | --- |',
    ...report.rows.map((row) => `| ${row.name} | ${row.required ? 'yes' : 'no'} | ${safeCell(row.result)} |`),
    '',
    ...report.errors.map((error) => `- ${safeCell(error)}`),
    '',
    'This gate reports component checks, not deployment or business acceptance.',
    '',
  ].join('\n');
}

export function main(env = process.env) {
  let needs;
  try { needs = JSON.parse(env.NEEDS_JSON); } catch { throw new Error('NEEDS_JSON must be valid JSON'); }
  const report = evaluateGate(needs);
  const text = summary(report);
  console.log(text);
  if (env.GITHUB_STEP_SUMMARY) appendFileSync(env.GITHUB_STEP_SUMMARY, text, 'utf8');
  if (!report.passed) throw new Error('One or more required CI checks did not pass');
  return report;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); } catch (error) {
    console.error(`CI Gate failed: ${error.message}`);
    process.exitCode = 1;
  }
}
