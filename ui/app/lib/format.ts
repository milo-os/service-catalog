import type { Phase } from "./types";

type BadgeType =
  | "primary"
  | "secondary"
  | "tertiary"
  | "quaternary"
  | "info"
  | "warning"
  | "danger"
  | "success"
  | "muted";

type BadgeTheme = "solid" | "outline" | "light";

export function phaseBadgeProps(
  phase: string
): { type: BadgeType; theme: BadgeTheme; label: string } {
  switch (phase as Phase) {
    case "Published":
      // UI label "Active" matches the Pencil design even though API value is Published.
      return { type: "success", theme: "light", label: "Active" };
    case "Deprecated":
      return { type: "warning", theme: "light", label: "Deprecated" };
    case "Retired":
      return { type: "danger", theme: "light", label: "Retired" };
    case "Draft":
      return { type: "secondary", theme: "light", label: "Draft" };
    default:
      return { type: "muted", theme: "solid", label: phase };
  }
}

export function conditionBadgeProps(status: string): { type: BadgeType; theme: BadgeTheme } {
  switch (status) {
    case "True":
      return { type: "success", theme: "light" };
    case "False":
      return { type: "danger", theme: "light" };
    default:
      return { type: "muted", theme: "light" };
  }
}

export function relativeAge(timestamp: string | undefined): string {
  if (!timestamp) return "—";
  const diff = Date.now() - new Date(timestamp).getTime();
  if (Number.isNaN(diff) || diff < 0) return "—";
  const m = Math.floor(diff / 60_000);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

export function formatPublishedAt(
  publishedAt: string | undefined,
  phase: string | undefined
): string {
  if (!publishedAt || phase !== "Published") return "—";
  const d = new Date(publishedAt);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleDateString();
}

export function truncate(value: string | undefined, max = 80): string {
  if (!value) return "—";
  return value.length > max ? value.slice(0, max - 1) + "…" : value;
}

export function formatDate(timestamp: string | undefined): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleDateString();
}


const UNIT_LABELS: Record<string, string> = {
  // Bytes
  bit: "Bit",
  bits: "Bits",
  byte: "Byte",
  bytes: "Bytes",
  kilobyte: "Kilobyte",
  kilobytes: "Kilobytes",
  kb: "KB",
  megabyte: "Megabyte",
  megabytes: "Megabytes",
  mb: "MB",
  gigabyte: "Gigabyte",
  gigabytes: "Gigabytes",
  gb: "GB",
  terabyte: "Terabyte",
  terabytes: "Terabytes",
  tb: "TB",
  petabyte: "Petabyte",
  petabytes: "Petabytes",
  pb: "PB",
  kibibyte: "Kibibyte",
  kibibytes: "Kibibytes",
  kib: "KiB",
  mebibyte: "Mebibyte",
  mebibytes: "Mebibytes",
  mib: "MiB",
  gibibyte: "Gibibyte",
  gibibytes: "Gibibytes",
  gib: "GiB",
  tebibyte: "Tebibyte",
  tebibytes: "Tebibytes",
  tib: "TiB",
  // Time
  nanosecond: "Nanosecond",
  nanoseconds: "Nanoseconds",
  ns: "ns",
  microsecond: "Microsecond",
  microseconds: "Microseconds",
  us: "µs",
  millisecond: "Millisecond",
  milliseconds: "Milliseconds",
  ms: "ms",
  second: "Second",
  seconds: "Seconds",
  s: "s",
  minute: "Minute",
  minutes: "Minutes",
  min: "min",
  hour: "Hour",
  hours: "Hours",
  hr: "hr",
  day: "Day",
  days: "Days",
  month: "Month",
  months: "Months",
  year: "Year",
  years: "Years",
  // Compute
  vcpu: "vCPU",
  vcpus: "vCPUs",
  "vcpu-second": "vCPU-second",
  "vcpu-seconds": "vCPU-seconds",
  "vcpu-hour": "vCPU-hour",
  "vcpu-hours": "vCPU-hours",
  core: "Core",
  cores: "Cores",
  "core-second": "Core-second",
  "core-seconds": "Core-seconds",
  "core-hour": "Core-hour",
  "core-hours": "Core-hours",
  // Memory
  "byte-second": "Byte-second",
  "byte-seconds": "Byte-seconds",
  "gb-second": "GB-second",
  "gb-seconds": "GB-seconds",
  "gib-second": "GiB-second",
  "gib-seconds": "GiB-seconds",
  // Requests / operations
  request: "Request",
  requests: "Requests",
  req: "Request",
  reqs: "Requests",
  operation: "Operation",
  operations: "Operations",
  op: "Operation",
  ops: "Operations",
  call: "Call",
  calls: "Calls",
  query: "Query",
  queries: "Queries",
  transaction: "Transaction",
  transactions: "Transactions",
  event: "Event",
  events: "Events",
  message: "Message",
  messages: "Messages",
  record: "Record",
  records: "Records",
  row: "Row",
  rows: "Rows",
  item: "Item",
  items: "Items",
  unit: "Unit",
  units: "Units",
  token: "Token",
  tokens: "Tokens",
  // Networking
  packet: "Packet",
  packets: "Packets",
  connection: "Connection",
  connections: "Connections",
};

export function formatUnit(unit: string | undefined, displayName?: string): string {
  if (!unit) return "—";
  return displayName || UNIT_LABELS[unit.toLowerCase()] || unit;
}
