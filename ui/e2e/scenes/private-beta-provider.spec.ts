import { execSync } from 'child_process';
import path from 'path';
import { fileURLToPath } from 'url';
import { test } from '../fixtures';

// Provider approval workflow
//
// This scene runs entirely from the provider's perspective. A pending
// ServiceConsumer is seeded directly in beforeAll (bypassing the multicluster
// entitlement-to-consumer flow that requires real project clusters). The scene
// shows a provider discovering a pending request, reviewing the details,
// writing an approval note, and approving access.

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const KUBECONFIG =
  process.env.KUBECONFIG ?? path.join(REPO_ROOT, '.test-infra/kubeconfig');

const CONSUMER_NAME = 'ai-platform-miloapis-com-local';

const CONSUMER_YAML = `\
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceConsumer
metadata:
  name: ${CONSUMER_NAME}
spec:
  serviceRef:
    name: ai-platform-miloapis-com
  consumerProjectRef:
    name: local
`;

test.beforeAll(() => {
  // The ServiceConsumer webhook only allows in-cluster service accounts to
  // create ServiceConsumers. Impersonate the controller service account so
  // this seed step passes the webhook check.
  const AS_CONTROLLER =
    '--as=system:serviceaccount:services-system:services-controller-manager';

  execSync(`kubectl apply ${AS_CONTROLLER} -f -`, {
    input: CONSUMER_YAML,
    env: { ...process.env, KUBECONFIG },
    stdio: ['pipe', 'pipe', 'pipe'],
  });

  // Set status.phase = PendingApproval — mimics what the entitlement
  // controller sets for GatedByProvider services. Done via the status
  // subresource so it survives controller no-ops (the controller skips
  // consumers with no Spec.Approval set).
  execSync(
    `kubectl patch serviceconsumer ${CONSUMER_NAME} --subresource=status --type=merge -p '{"status":{"phase":"PendingApproval"}}'`,
    {
      env: { ...process.env, KUBECONFIG },
      stdio: ['pipe', 'pipe', 'pipe'],
    }
  );
});

test('provider approval workflow', async ({ page }) => {
  // ── Access requests list ──────────────────────────────────────────────────
  await page.goto('/consumers');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);

  // The seeded ServiceConsumer should appear immediately as PendingApproval.
  // Poll briefly in case the UI needs a moment to reflect the seeded state.
  const reviewLink = page.getByRole('link', { name: /review/i }).first();
  for (let i = 0; i < 3; i++) {
    if (await reviewLink.isVisible({ timeout: 1_000 }).catch(() => false)) break;
    await page.waitForTimeout(2_000);
    await page.reload();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1_500);
  }

  // ── Pending request callout — let viewer read the summary ─────────────────
  const pendingCallout = page.getByText(/pending request/i).first();
  if (await pendingCallout.isVisible({ timeout: 2_000 }).catch(() => false)) {
    await page.waitForTimeout(3_000);
  } else {
    await page.waitForTimeout(2_000);
  }

  // ── Click Review to open the request detail ───────────────────────────────
  if (!(await reviewLink.isVisible({ timeout: 3_000 }).catch(() => false))) {
    // Nothing pending — the scene cannot proceed; stop gracefully.
    return;
  }

  await reviewLink.click();
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(3_000);

  // ── Request details — let viewer read consumer project and service ─────────
  // Pause so the viewer can read the consumer project name, service name,
  // request date, and any conditions shown on this page.
  await page.waitForTimeout(3_000);

  // ── Fill approval message ──────────────────────────────────────────────────
  const messageField = page.locator('textarea').first();
  if (await messageField.isVisible({ timeout: 2_000 }).catch(() => false)) {
    await messageField.click();
    await messageField.pressSequentially(
      'Welcome to the AI Platform private beta. Your access is now active.',
      { delay: 45 }
    );
    await page.waitForTimeout(1_500);
  }

  // ── Approve ────────────────────────────────────────────────────────────────
  const approveBtn = page.getByRole('button', { name: /approve/i });
  if (!(await approveBtn.isVisible({ timeout: 3_000 }).catch(() => false))) return;

  await approveBtn.click();
  await page.waitForLoadState('networkidle');

  // Simulate async controller reconciliation: patch status to Active after
  // spec.approval is set. In production the service-consumer controller does
  // this; in the e2e kind cluster the multicluster provider clusters aren't
  // engaged so we drive it directly.
  try {
    execSync(
      `kubectl patch serviceconsumer ${CONSUMER_NAME} --subresource=status --type=merge -p '{"status":{"phase":"Active"}}'`,
      { env: { ...process.env, KUBECONFIG }, stdio: ['pipe', 'pipe', 'pipe'] }
    );
  } catch {
    // Non-fatal — the polling loop below handles missing Active status gracefully.
  }

  await page.waitForTimeout(3_000);

  // ── Back on /consumers — poll until consumer row shows Active ─────────────
  // The controller reconciles asynchronously after spec.approval is set.
  // Reload until "Active" appears in the status column (up to ~20 s).
  let activeBadge = page.getByText(/^Active$/).first();
  for (let i = 0; i < 4; i++) {
    if (await activeBadge.isVisible({ timeout: 1_000 }).catch(() => false)) break;
    await page.waitForTimeout(4_000);
    await page.reload();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2_000);
    activeBadge = page.getByText(/^Active$/).first();
  }

  if (await activeBadge.isVisible({ timeout: 1_000 }).catch(() => false)) {
    await activeBadge.scrollIntoViewIfNeeded();
    await page.waitForTimeout(3_000);
  } else {
    await page.waitForTimeout(2_000);
  }
});
