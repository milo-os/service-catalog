export type DiffState = "unchanged" | "added" | "removed" | "modified";

export type DiffEntry<T> =
  | { state: "unchanged"; key: string; left: T; right: T }
  | { state: "added"; key: string; right: T }
  | { state: "removed"; key: string; left: T }
  | {
      state: "modified";
      key: string;
      left: T;
      right: T;
      fields: string[];
    };

function valueEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a == null && b == null) return true;
  if (a == null || b == null) return false;
  if (typeof a !== "object" && typeof b !== "object") return a === b;
  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false;
  }
}

/**
 * Compute a per-key diff between two arrays. Each item is identified by
 * `keyOf`, and the field-level comparison runs over the entries returned
 * by `fieldsOf`. Items that appear on only one side become `added` /
 * `removed`; items present on both sides are `unchanged` if every field
 * is equal (deep), otherwise `modified` with the changed field names.
 */
export function diffByKey<T>(
  left: T[] | undefined,
  right: T[] | undefined,
  keyOf: (t: T) => string,
  fieldsOf: (t: T) => Record<string, unknown>
): DiffEntry<T>[] {
  const leftMap = new Map<string, T>();
  for (const t of left ?? []) leftMap.set(keyOf(t), t);
  const rightMap = new Map<string, T>();
  for (const t of right ?? []) rightMap.set(keyOf(t), t);

  const allKeys = new Set<string>([
    ...leftMap.keys(),
    ...rightMap.keys(),
  ]);
  const result: DiffEntry<T>[] = [];
  for (const key of allKeys) {
    const l = leftMap.get(key);
    const r = rightMap.get(key);
    if (l !== undefined && r !== undefined) {
      const lf = fieldsOf(l);
      const rf = fieldsOf(r);
      const fieldKeys = new Set<string>([
        ...Object.keys(lf),
        ...Object.keys(rf),
      ]);
      const diffFields: string[] = [];
      for (const f of fieldKeys) {
        if (!valueEqual(lf[f], rf[f])) diffFields.push(f);
      }
      if (diffFields.length === 0) {
        result.push({ state: "unchanged", key, left: l, right: r });
      } else {
        result.push({
          state: "modified",
          key,
          left: l,
          right: r,
          fields: diffFields,
        });
      }
    } else if (l !== undefined) {
      result.push({ state: "removed", key, left: l });
    } else if (r !== undefined) {
      result.push({ state: "added", key, right: r });
    }
  }
  return result;
}

const STATE_ORDER: Record<DiffState, number> = {
  removed: 0,
  added: 1,
  modified: 2,
  unchanged: 3,
};

/** Sort: removed → added → modified → unchanged, alphabetical by key inside each group. */
export function sortDiff<T>(diff: DiffEntry<T>[]): DiffEntry<T>[] {
  return [...diff].sort((a, b) => {
    const orderDiff = STATE_ORDER[a.state] - STATE_ORDER[b.state];
    if (orderDiff !== 0) return orderDiff;
    return a.key.localeCompare(b.key);
  });
}

export interface DiffCounts {
  added: number;
  removed: number;
  modified: number;
  unchanged: number;
}

export function countDiff<T>(diff: DiffEntry<T>[]): DiffCounts {
  const counts: DiffCounts = {
    added: 0,
    removed: 0,
    modified: 0,
    unchanged: 0,
  };
  for (const e of diff) counts[e.state]++;
  return counts;
}

export function totalChanged(counts: DiffCounts): number {
  return counts.added + counts.removed + counts.modified;
}
