/**
 * Reads scenario YAML files from e2e/scenarios/, checks which ones match
 * the files changed in this PR (via git diff), and prints a JSON array of
 * matched scenarios to stdout. Falls back to the scenario marked
 * `fallback: true` when no specific scenario matches.
 *
 * Usage:
 *   node e2e/resolve-scenarios.mjs         # from ui/
 *   GITHUB_BASE_REF=main node ...          # set automatically in CI
 */
import { readFileSync, readdirSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';
import { execSync } from 'child_process';
import { load } from 'js-yaml';

const __dirname = dirname(fileURLToPath(import.meta.url));

function globToRegex(glob) {
  const escaped = glob
    .replace(/[.+^${}()|[\]\\]/g, '\\$&')
    .replace(/\*\*/g, '\x00')
    .replace(/\*/g, '[^/]*')
    .replace(/\x00/g, '.*');
  return new RegExp(`^${escaped}$`);
}

function anyFileMatchesPattern(files, pattern) {
  const re = globToRegex(pattern);
  return files.some(f => re.test(f));
}

// Resolve changed files from git diff
const baseRef = process.env.GITHUB_BASE_REF || 'main';
let changedFiles = [];
try {
  const out = execSync(`git diff --name-only origin/${baseRef}...HEAD 2>/dev/null`, {
    encoding: 'utf8',
  });
  changedFiles = out.trim().split('\n').filter(Boolean);
} catch {
  // Local dev without a remote, or shallow clone — treat as no changed files
  // so the fallback scenario is always used.
}

// Load scenario definitions
const scenariosDir = join(__dirname, 'scenarios');
const scenarios = readdirSync(scenariosDir)
  .filter(f => f.endsWith('.yaml') || f.endsWith('.yml'))
  .map(f => load(readFileSync(join(scenariosDir, f), 'utf8')));

const fallback = scenarios.find(s => s.fallback === true);
const specific = scenarios.filter(s => !s.fallback);

// Match scenarios against changed files
const matched = changedFiles.length > 0
  ? specific.filter(s =>
      (s.match ?? []).some(pattern => anyFileMatchesPattern(changedFiles, pattern))
    )
  : [];

const result = matched.length > 0 ? matched : (fallback ? [fallback] : specific);

process.stdout.write(
  JSON.stringify(result.map(({ name, description, spec }) => ({ name, description, spec }))) + '\n'
);
