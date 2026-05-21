/**
 * Shared validation utilities for wizard forms (create-service and
 * create-configuration wizards). Pure functions — safe to import from
 * either client components or Remix action handlers.
 */

export interface MrtDraft {
  type: string;
  displayName: string;
  description: string;
  gvk: { group: string; kind: string };
  labels: string[];
}

export interface MeterDraft {
  name: string;
  displayName: string;
  description: string;
  measurement: { aggregation: string; unit: string; unitDisplayName: string };
  billing: {
    consumedUnit: string;
    consumedUnitDisplayName: string;
    pricingUnit: string;
    pricingUnitDisplayName: string;
  };
  monitoredResourceTypes: string[];
}

export const NAME_RE = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
export const KIND_RE = /^[A-Z][A-Za-z0-9]*$/;
export const SUBDOMAIN_RE =
  /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
export const SEMVER_RE = /^\d+\.\d+\.\d+$/;

export const AGGREGATIONS = ["Sum", "Max", "Average", "Count"] as const;
export type Aggregation = (typeof AGGREGATIONS)[number];

export function newMrt(): MrtDraft {
  return {
    type: "",
    displayName: "",
    description: "",
    gvk: { group: "", kind: "" },
    labels: [],
  };
}

export function newMeter(): MeterDraft {
  return {
    name: "",
    displayName: "",
    description: "",
    measurement: { aggregation: "Sum", unit: "", unitDisplayName: "" },
    billing: {
      consumedUnit: "",
      consumedUnitDisplayName: "",
      pricingUnit: "",
      pricingUnitDisplayName: "",
    },
    monitoredResourceTypes: [],
  };
}

/**
 * Validate the array of MRT drafts. Returns an error map keyed by
 * `mrt-${index}-${field}`.
 */
export function validateMrts(mrts: MrtDraft[]): Record<string, string> {
  const errors: Record<string, string> = {};
  const seen = new Set<string>();
  mrts.forEach((mrt, i) => {
    if (!NAME_RE.test(mrt.type)) {
      errors[`mrt-${i}-type`] =
        "Type must be lowercase alphanumeric (with hyphens), start with a letter.";
    } else if (seen.has(mrt.type)) {
      errors[`mrt-${i}-type`] = "Duplicate type.";
    } else {
      seen.add(mrt.type);
    }
    if (mrt.displayName.trim().length < 1 || mrt.displayName.length > 120) {
      errors[`mrt-${i}-displayName`] =
        "Display name must be 1–120 characters.";
    }
    if (mrt.description.length > 500) {
      errors[`mrt-${i}-description`] =
        "Description must be 500 characters or fewer.";
    }
    if (!SUBDOMAIN_RE.test(mrt.gvk.group)) {
      errors[`mrt-${i}-group`] =
        "GVK group must be a DNS subdomain (e.g. compute.miloapis.com).";
    }
    if (!KIND_RE.test(mrt.gvk.kind)) {
      errors[`mrt-${i}-kind`] = "GVK kind must be PascalCase.";
    }
  });
  return errors;
}

/**
 * Validate the array of meter drafts. When `mrts.length > 0`, every
 * meter must reference at least one MRT (the soft-coupling rule).
 * Returns an error map keyed by `meter-${index}-${field}`.
 */
export function validateMeters(
  meters: MeterDraft[],
  mrts: MrtDraft[]
): Record<string, string> {
  const errors: Record<string, string> = {};
  const seen = new Set<string>();
  const requireMrtBinding = mrts.length > 0;
  meters.forEach((m, i) => {
    if (!NAME_RE.test(m.name)) {
      errors[`meter-${i}-name`] =
        "Name must be lowercase alphanumeric (with hyphens), start with a letter.";
    } else if (seen.has(m.name)) {
      errors[`meter-${i}-name`] = "Duplicate name.";
    } else {
      seen.add(m.name);
    }
    if (m.displayName.trim().length < 1 || m.displayName.length > 120) {
      errors[`meter-${i}-displayName`] =
        "Display name must be 1–120 characters.";
    }
    if (m.description.length > 500) {
      errors[`meter-${i}-description`] =
        "Description must be 500 characters or fewer.";
    }
    if (!AGGREGATIONS.includes(m.measurement.aggregation as Aggregation)) {
      errors[`meter-${i}-aggregation`] =
        "Aggregation must be Sum / Max / Average / Count.";
    }
    if (!m.measurement.unit.trim()) {
      errors[`meter-${i}-unit`] = "Measurement unit is required.";
    }
    if (!m.billing.consumedUnit.trim()) {
      errors[`meter-${i}-consumedUnit`] = "Consumed unit is required.";
    }
    if (!m.billing.pricingUnit.trim()) {
      errors[`meter-${i}-pricingUnit`] = "Pricing unit is required.";
    }
    if (requireMrtBinding && m.monitoredResourceTypes.length === 0) {
      errors[`meter-${i}-monitoredResourceTypes`] =
        "Bind at least one monitored resource.";
    }
  });
  return errors;
}

export interface ConfigVersionInput {
  version: string;
  source: "blank" | "clone";
  cloneFrom?: string;
}

/**
 * Validate the version + source pickers from the create-configuration
 * wizard's step 1.
 *
 * - `existingVersions` is the list of `spec.version` values for this
 *   service so we can reject duplicates (case-insensitive).
 * - `cloneOptions` is the list of `metadata.name`s that are valid
 *   clone-source choices.
 */
export function validateConfigVersion(
  input: ConfigVersionInput,
  existingVersions: string[],
  cloneOptions: string[]
): Record<string, string> {
  const errors: Record<string, string> = {};
  const v = input.version.trim();
  if (!v) {
    errors.version = "Version is required.";
  } else if (!SEMVER_RE.test(v)) {
    errors.version = "Version must be major.minor.patch (e.g. 1.0.0).";
  } else if (v.length > 32) {
    errors.version = "Version must be 32 characters or fewer.";
  } else if (
    existingVersions.some((e) => e.toLowerCase() === v.toLowerCase())
  ) {
    errors.version = `Version ${v} already exists for this service.`;
  }
  if (input.source !== "blank" && input.source !== "clone") {
    errors.source = "Pick a starting source.";
  }
  if (input.source === "clone") {
    if (!input.cloneFrom) {
      errors.cloneFrom = "Pick a version to clone from.";
    } else if (!cloneOptions.includes(input.cloneFrom)) {
      errors.cloneFrom = "Invalid clone source.";
    }
  }
  return errors;
}

/**
 * Suggest the next version. If `prev` parses as `major.minor.patch`,
 * returns `${major}.${minor + 1}.0`. Otherwise returns `1.0.0`.
 */
export function suggestNextVersion(prev: string | null | undefined): string {
  if (!prev) return "1.0.0";
  const m = prev.match(/^(\d+)\.(\d+)\.(\d+)$/);
  if (!m) return "1.0.0";
  const major = parseInt(m[1], 10);
  const minor = parseInt(m[2], 10);
  return `${major}.${minor + 1}.0`;
}
