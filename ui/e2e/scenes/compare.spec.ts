import { execSync } from 'child_process';
import path from 'path';
import { fileURLToPath } from 'url';
import { test, expect } from '../fixtures';

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const KUBECONFIG =
  process.env.KUBECONFIG ?? path.join(REPO_ROOT, '.test-infra/kubeconfig');

const CONFIG_V1_YAML = `\
apiVersion: services.miloapis.com/v1alpha1
kind: ServiceConfiguration
metadata:
  name: compute-miloapis-com-v1
spec:
  serviceRef:
    name: compute-miloapis-com
  phase: Deprecated
  monitoredResourceTypes:
    - type: compute.miloapis.com/Instance
      displayName: Compute Instance
      description: A virtual machine instance running in a Milo zone.
      gvk:
        group: compute.miloapis.com
        kind: Instance
      labels:
        - name: region
          description: The geographic region the instance runs in.
        - name: zone
          description: The availability zone within the region.
  metrics:
    - name: compute.miloapis.com/instance/cpu-seconds
      displayName: vCPU Seconds
      description: Accumulated vCPU-seconds consumed by running instances.
      kind: Cumulative
      unit: s
    - name: compute.miloapis.com/instance/memory-byte-seconds
      displayName: Memory Byte-Seconds
      description: Accumulated RAM byte-seconds consumed by running instances.
      kind: Cumulative
      unit: By.s
  billing:
    consumerDestinations:
      - monitoredResourceType: compute.miloapis.com/Instance
        metrics:
          - compute.miloapis.com/instance/cpu-seconds
          - compute.miloapis.com/instance/memory-byte-seconds
`;

test.beforeAll(() => {
  execSync('kubectl apply -f -', {
    input: CONFIG_V1_YAML,
    env: { ...process.env, KUBECONFIG },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
});

test('configuration compare', async ({ page }) => {
  // ── Navigate to compare from the configurations tab ───────────────────────
  await page.goto('/services/compute-miloapis-com?tab=configurations');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2_500);

  // ── Compare screen — empty pickers ───────────────────────────────────────
  await page.goto('/services/compute-miloapis-com/configurations/compare');
  await page.waitForLoadState('networkidle');
  await expect(
    page.locator('[data-e2e="page-title"]')
  ).toContainText(/compare configurations/i, { timeout: 10_000 });
  await page.waitForTimeout(3_000);

  // ── Populated diff (requires seed configs) ────────────────────────────────
  const left = 'compute-miloapis-com';
  const right = 'compute-miloapis-com-v1';

  const probe = await page.request.get(
    '/apis/services.miloapis.com/v1alpha1/serviceconfigurations'
  );
  if (probe.ok()) {
    const list = await probe.json().catch(() => null) as { items?: Array<{ metadata?: { name?: string } }> } | null;
    const names = new Set((list?.items ?? []).map(c => c.metadata?.name).filter(Boolean));

    if (names.has(left) && names.has(right)) {
      await page.goto(
        `/services/${left}/configurations/compare?left=${left}&right=${right}`
      );
      await page.waitForLoadState('networkidle');
      await page.waitForTimeout(4_000);
    }
  }
});
