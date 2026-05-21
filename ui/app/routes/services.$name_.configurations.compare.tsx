import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import {
  Link,
  useLoaderData,
  useNavigate,
  useParams,
} from "@remix-run/react";
import { useMemo } from "react";
import {
  Alert,
  AlertDescription,
  AlertTitle,
} from "@datum-cloud/datum-ui/alert";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@datum-cloud/datum-ui/select";
import { ArrowLeftRight, GitCompare } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import {
  countDiff,
  diffByKey,
  sortDiff,
  totalChanged,
  type DiffEntry,
} from "~/lib/diff";
import { formatDate, formatUnit, phaseBadgeProps } from "~/lib/format";
import type {
  KubeList,
  MeterSpec,
  MonitoredResourceLabel,
  MonitoredResourceTypeSpec,
  Service,
  ServiceConfiguration,
} from "~/lib/types";

interface LoaderData {
  service?: Service;
  configs: ServiceConfiguration[];
  left?: ServiceConfiguration;
  right?: ServiceConfiguration;
  leftName?: string;
  rightName?: string;
  error?: string;
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const name = params.name;
  const url = new URL(request.url);
  const leftName = url.searchParams.get("left") ?? undefined;
  const rightName = url.searchParams.get("right") ?? undefined;
  if (!name) {
    return json(
      {
        error: "Missing service name",
        configs: [],
        leftName,
        rightName,
      } satisfies LoaderData,
      { status: 400 }
    );
  }
  try {
    const [service, configList] = await Promise.all([
      fetchK8s<Service>(
        request,
        `/apis/services.miloapis.com/v1alpha1/services/${encodeURIComponent(name)}`
      ),
      fetchK8s<KubeList<ServiceConfiguration>>(
        request,
        `/apis/services.miloapis.com/v1alpha1/serviceconfigurations`
      ),
    ]);
    const mine = (configList.items ?? []).filter(
      (c) => c.spec?.serviceRef?.name === service.metadata.name
    );
    // Sort by publishedAt desc (newest first), falling back to creationTimestamp.
    mine.sort((a, b) => {
      const at = a.status?.publishedAt ?? a.metadata.creationTimestamp;
      const bt = b.status?.publishedAt ?? b.metadata.creationTimestamp;
      return new Date(bt).getTime() - new Date(at).getTime();
    });
    const left = leftName
      ? mine.find((c) => c.metadata.name === leftName)
      : undefined;
    const right = rightName
      ? mine.find((c) => c.metadata.name === rightName)
      : undefined;
    return json({
      service,
      configs: mine,
      left,
      right,
      leftName,
      rightName,
    } satisfies LoaderData);
  } catch (e) {
    return json({
      error: e instanceof Error ? e.message : String(e),
      configs: [],
      leftName,
      rightName,
    } satisfies LoaderData);
  }
}

const MRT_FIELD_LABELS: Record<string, string> = {
  type: "Type",
  displayName: "Display name",
  description: "Description",
  group: "GVK group",
  kind: "GVK kind",
  labels: "Labels",
};

const METER_FIELD_LABELS: Record<string, string> = {
  name: "Name",
  displayName: "Display name",
  description: "Description",
  aggregation: "Aggregation",
  unit: "Unit",
  consumedUnit: "Billing — consumed unit",
  pricingUnit: "Billing — pricing unit",
  monitoredResourceTypes: "Bound monitored resources",
};

function mrtFields(
  mrt: MonitoredResourceTypeSpec
): Record<string, unknown> {
  return {
    type: mrt.type ?? "",
    displayName: mrt.displayName ?? "",
    description: mrt.description ?? "",
    group: mrt.gvk?.group ?? "",
    kind: mrt.gvk?.kind ?? "",
    labels: (mrt.labels ?? []).map((l) => ({
      name: l.name,
      description: l.description ?? "",
    })),
  };
}

function meterFields(m: MeterSpec): Record<string, unknown> {
  return {
    name: m.name ?? "",
    displayName: m.displayName ?? "",
    description: m.description ?? "",
    aggregation: m.measurement?.aggregation ?? "",
    unit: m.measurement?.unit ?? "",
    consumedUnit: m.billing?.consumedUnit ?? "",
    pricingUnit: m.billing?.pricingUnit ?? "",
    monitoredResourceTypes: [...(m.monitoredResourceTypes ?? [])].sort(),
  };
}

function ConfigOptionLabel({ cfg }: { cfg: ServiceConfiguration }) {
  const phase = phaseBadgeProps(cfg.spec.phase);
  const version = cfg.spec.version
    ? `v${cfg.spec.version}`
    : cfg.metadata.name;
  return `${version} (${phase.label})`;
}

function StateBadge({ state }: { state: DiffEntry<unknown>["state"] }) {
  if (state === "added")
    return (
      <Badge type="success" theme="light">
        Added
      </Badge>
    );
  if (state === "removed")
    return (
      <Badge type="danger" theme="light">
        Removed
      </Badge>
    );
  return (
    <Badge type="warning" theme="light">
      Modified
    </Badge>
  );
}

function ScalarDiffRow({
  label,
  equal,
  left,
  right,
  renderAs,
}: {
  label: string;
  /**
   * Pre-computed equality on the underlying scalar source. Required
   * because `left`/`right` may be React nodes (badges) whose JSON
   * serialization is unreliable.
   */
  equal: boolean;
  left: React.ReactNode;
  right: React.ReactNode;
  renderAs?: "text" | "badge";
}) {
  const leftCellClass = equal
    ? "text-muted-foreground"
    : "bg-destructive/10 text-destructive rounded px-2 py-1";
  const rightCellClass = equal
    ? "text-muted-foreground"
    : "bg-success-100 text-success-700 dark:bg-success-100/20 rounded px-2 py-1";
  return (
    <div className="grid grid-cols-[180px_1fr_1fr] gap-3 items-start py-2 border-b border-border/50 last:border-b-0">
      <div className="text-sm font-medium text-muted-foreground pt-1">
        {label}
      </div>
      <div className={`text-sm ${leftCellClass}`}>
        {renderAs === "badge" ? (
          left
        ) : (
          <span className="font-mono text-xs whitespace-pre-line">
            {left || "—"}
          </span>
        )}
      </div>
      <div className={`text-sm ${rightCellClass}`}>
        {renderAs === "badge" ? (
          right
        ) : (
          <span className="font-mono text-xs whitespace-pre-line">
            {right || "—"}
          </span>
        )}
      </div>
    </div>
  );
}

function PhaseBadge({ phase }: { phase: string }) {
  const p = phaseBadgeProps(phase);
  return (
    <Badge type={p.type} theme={p.theme}>
      {p.label}
    </Badge>
  );
}

function LabelsList({
  labels,
}: {
  labels: MonitoredResourceLabel[] | undefined;
}) {
  if (!labels || labels.length === 0) return <span>—</span>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {labels.map((l) => (
        <Badge
          key={l.name}
          type="secondary"
          theme="light"
          title={l.description}
        >
          {l.name}
        </Badge>
      ))}
    </div>
  );
}

function ChipsList({ items }: { items: string[] | undefined }) {
  if (!items || items.length === 0) return <span>—</span>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {items.map((s) => (
        <Badge key={s} type="secondary" theme="light">
          {s}
        </Badge>
      ))}
    </div>
  );
}

function MrtFieldValue({
  field,
  mrt,
}: {
  field: string;
  mrt: MonitoredResourceTypeSpec | undefined;
}) {
  if (!mrt) return <span>—</span>;
  switch (field) {
    case "type":
      return <span className="font-mono text-xs">{mrt.type}</span>;
    case "displayName":
      return <span>{mrt.displayName ?? "—"}</span>;
    case "description":
      return <span className="whitespace-pre-line">{mrt.description ?? "—"}</span>;
    case "group":
      return <span className="font-mono text-xs">{mrt.gvk?.group ?? "—"}</span>;
    case "kind":
      return <span className="font-mono text-xs">{mrt.gvk?.kind ?? "—"}</span>;
    case "labels":
      return <LabelsList labels={mrt.labels} />;
    default:
      return <span>—</span>;
  }
}

function MeterFieldValue({
  field,
  meter,
}: {
  field: string;
  meter: MeterSpec | undefined;
}) {
  if (!meter) return <span>—</span>;
  switch (field) {
    case "name":
      return <span className="font-mono text-xs">{meter.name}</span>;
    case "displayName":
      return <span>{meter.displayName ?? "—"}</span>;
    case "description":
      return (
        <span className="whitespace-pre-line">{meter.description ?? "—"}</span>
      );
    case "aggregation":
      return (
        <span className="font-mono text-xs">
          {meter.measurement?.aggregation ?? "—"}
        </span>
      );
    case "unit":
      return (
        <span>
          {formatUnit(meter.measurement?.unit, meter.measurement?.unitDisplayName)}
        </span>
      );
    case "consumedUnit":
      return (
        <span>
          {formatUnit(meter.billing?.consumedUnit, meter.billing?.consumedUnitDisplayName)}
        </span>
      );
    case "pricingUnit":
      return (
        <span>
          {formatUnit(meter.billing?.pricingUnit, meter.billing?.pricingUnitDisplayName)}
        </span>
      );
    case "monitoredResourceTypes":
      return <ChipsList items={meter.monitoredResourceTypes} />;
    default:
      return <span>—</span>;
  }
}

function MrtDiffEntry({
  entry,
}: {
  entry: DiffEntry<MonitoredResourceTypeSpec>;
}) {
  if (entry.state === "unchanged") return null;
  const left = "left" in entry ? entry.left : undefined;
  const right = "right" in entry ? entry.right : undefined;
  const fields =
    entry.state === "modified"
      ? entry.fields
      : Object.keys(MRT_FIELD_LABELS);
  const titleLeft = left?.type ?? "—";
  const titleRight = right?.type ?? "—";
  const headerType =
    entry.state === "added"
      ? titleRight
      : entry.state === "removed"
      ? titleLeft
      : titleLeft || titleRight;

  return (
    <div className="border border-border/60 rounded-md p-3 flex flex-col gap-2">
      <div className="flex items-center gap-3">
        <span className="font-mono text-sm text-foreground">
          {headerType}
        </span>
        <StateBadge state={entry.state} />
      </div>
      <div className="flex flex-col">
        <div className="grid grid-cols-[180px_1fr_1fr] gap-3 text-xs uppercase tracking-wider text-muted-foreground border-b border-border/50 pb-2">
          <span>Field</span>
          <span>Left</span>
          <span>Right</span>
        </div>
        {fields.map((field) => {
          const leftCellClass =
            entry.state === "added"
              ? "text-muted-foreground"
              : "bg-destructive/10 text-destructive rounded px-2 py-1";
          const rightCellClass =
            entry.state === "removed"
              ? "text-muted-foreground"
              : "bg-success-100 text-success-700 dark:bg-success-100/20 rounded px-2 py-1";
          return (
            <div
              key={field}
              className="grid grid-cols-[180px_1fr_1fr] gap-3 items-start py-2 border-b border-border/50 last:border-b-0"
            >
              <div className="text-sm font-medium text-muted-foreground pt-1">
                {MRT_FIELD_LABELS[field] ?? field}
              </div>
              <div
                className={`text-sm ${
                  entry.state === "added" ? "text-muted-foreground" : leftCellClass
                }`}
              >
                {entry.state === "added" ? (
                  <span>—</span>
                ) : (
                  <MrtFieldValue field={field} mrt={left} />
                )}
              </div>
              <div
                className={`text-sm ${
                  entry.state === "removed"
                    ? "text-muted-foreground"
                    : rightCellClass
                }`}
              >
                {entry.state === "removed" ? (
                  <span>—</span>
                ) : (
                  <MrtFieldValue field={field} mrt={right} />
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function MeterDiffEntry({ entry }: { entry: DiffEntry<MeterSpec> }) {
  if (entry.state === "unchanged") return null;
  const left = "left" in entry ? entry.left : undefined;
  const right = "right" in entry ? entry.right : undefined;
  const fields =
    entry.state === "modified"
      ? entry.fields
      : Object.keys(METER_FIELD_LABELS);
  const titleLeft = left?.name ?? "—";
  const titleRight = right?.name ?? "—";
  const headerName =
    entry.state === "added"
      ? titleRight
      : entry.state === "removed"
      ? titleLeft
      : titleLeft || titleRight;

  return (
    <div className="border border-border/60 rounded-md p-3 flex flex-col gap-2">
      <div className="flex items-center gap-3">
        <span className="font-mono text-sm text-foreground">{headerName}</span>
        <StateBadge state={entry.state} />
      </div>
      <div className="flex flex-col">
        <div className="grid grid-cols-[180px_1fr_1fr] gap-3 text-xs uppercase tracking-wider text-muted-foreground border-b border-border/50 pb-2">
          <span>Field</span>
          <span>Left</span>
          <span>Right</span>
        </div>
        {fields.map((field) => {
          const leftCellClass =
            entry.state === "added"
              ? "text-muted-foreground"
              : "bg-destructive/10 text-destructive rounded px-2 py-1";
          const rightCellClass =
            entry.state === "removed"
              ? "text-muted-foreground"
              : "bg-success-100 text-success-700 dark:bg-success-100/20 rounded px-2 py-1";
          return (
            <div
              key={field}
              className="grid grid-cols-[180px_1fr_1fr] gap-3 items-start py-2 border-b border-border/50 last:border-b-0"
            >
              <div className="text-sm font-medium text-muted-foreground pt-1">
                {METER_FIELD_LABELS[field] ?? field}
              </div>
              <div
                className={`text-sm ${
                  entry.state === "added"
                    ? "text-muted-foreground"
                    : leftCellClass
                }`}
              >
                {entry.state === "added" ? (
                  <span>—</span>
                ) : (
                  <MeterFieldValue field={field} meter={left} />
                )}
              </div>
              <div
                className={`text-sm ${
                  entry.state === "removed"
                    ? "text-muted-foreground"
                    : rightCellClass
                }`}
              >
                {entry.state === "removed" ? (
                  <span>—</span>
                ) : (
                  <MeterFieldValue field={field} meter={right} />
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function buildSummary(
  mrtChanges: number,
  meterChanges: number,
  modifiedTotal: number,
  addedMrts: number,
  removedMrts: number,
  addedMeters: number,
  removedMeters: number
): string {
  if (mrtChanges + meterChanges === 0) {
    return "Configurations are identical.";
  }
  const parts: string[] = [];
  const mrtNet = addedMrts - removedMrts;
  if (mrtNet !== 0) {
    parts.push(
      `${mrtNet > 0 ? "+" : "−"}${Math.abs(mrtNet)} monitored resource${
        Math.abs(mrtNet) === 1 ? "" : "s"
      }`
    );
  } else if (addedMrts + removedMrts > 0) {
    parts.push(
      `${addedMrts + removedMrts} monitored resource change${
        addedMrts + removedMrts === 1 ? "" : "s"
      }`
    );
  }
  const meterNet = addedMeters - removedMeters;
  if (meterNet !== 0) {
    parts.push(
      `${meterNet > 0 ? "+" : "−"}${Math.abs(meterNet)} meter${
        Math.abs(meterNet) === 1 ? "" : "s"
      }`
    );
  } else if (addedMeters + removedMeters > 0) {
    parts.push(
      `${addedMeters + removedMeters} meter change${
        addedMeters + removedMeters === 1 ? "" : "s"
      }`
    );
  }
  if (modifiedTotal > 0) {
    parts.push(`${modifiedTotal} modified`);
  }
  return parts.join(" · ");
}

export default function ServiceConfigurationCompare() {
  const data = useLoaderData<typeof loader>() as LoaderData;
  const params = useParams();
  const navigate = useNavigate();
  const serviceParam = params.name ?? "";
  const backHref = `/services/${encodeURIComponent(serviceParam)}?tab=configurations`;

  const updateUrl = (next: { left?: string; right?: string }) => {
    const sp = new URLSearchParams();
    const leftVal = next.left ?? data.leftName ?? "";
    const rightVal = next.right ?? data.rightName ?? "";
    if (leftVal) sp.set("left", leftVal);
    if (rightVal) sp.set("right", rightVal);
    const qs = sp.toString();
    navigate(
      `/services/${encodeURIComponent(serviceParam)}/configurations/compare${
        qs ? `?${qs}` : ""
      }`,
      { replace: false }
    );
  };

  const swap = () => {
    if (!data.leftName || !data.rightName) return;
    updateUrl({ left: data.rightName, right: data.leftName });
  };

  const mrtDiff = useMemo(
    () =>
      sortDiff(
        diffByKey(
          data.left?.spec.monitoredResourceTypes,
          data.right?.spec.monitoredResourceTypes,
          (m) => m.type,
          mrtFields
        )
      ),
    [data.left, data.right]
  );

  const meterDiff = useMemo(
    () =>
      sortDiff(
        diffByKey(
          data.left?.spec.meters,
          data.right?.spec.meters,
          (m) => m.name,
          meterFields
        )
      ),
    [data.left, data.right]
  );

  if (data.error) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4">
        <Link to={backHref} className="text-sm text-primary hover:underline">
          ← Back to configurations
        </Link>
        <PageTitle
          title="Compare configurations"
          description=""
          actionsPosition="inline"
        />
        <Card>
          <CardHeader>
            <CardTitle>Failed to load data</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">{data.error}</p>
          </CardContent>
        </Card>
      </div>
    );
  }

  const haveBoth = !!data.left && !!data.right;
  const sameConfig =
    haveBoth && data.left?.metadata.name === data.right?.metadata.name;

  const mrtCounts = countDiff(mrtDiff);
  const meterCounts = countDiff(meterDiff);
  const summary = buildSummary(
    totalChanged(mrtCounts),
    totalChanged(meterCounts),
    mrtCounts.modified + meterCounts.modified,
    mrtCounts.added,
    mrtCounts.removed,
    meterCounts.added,
    meterCounts.removed
  );

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <Link to={backHref} className="text-sm text-primary hover:underline">
        ← Back to configurations
      </Link>

      <div className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-6 flex-wrap">
          <PageTitle
            title="Compare configurations"
            description={
              data.service?.spec.displayName ||
              data.service?.metadata.name ||
              ""
            }
            actionsPosition="inline"
          />
          <div className="flex items-center gap-3">
            <div className="flex flex-col gap-1">
              <span className="text-xs uppercase tracking-wider text-muted-foreground">
                Left
              </span>
              <Select
                value={data.leftName ?? ""}
                onValueChange={(value) => updateUrl({ left: value })}
              >
                <SelectTrigger className="min-w-[220px]">
                  <SelectValue placeholder="Select…" />
                </SelectTrigger>
                <SelectContent>
                  {data.configs.map((cfg) => (
                    <SelectItem
                      key={cfg.metadata.name}
                      value={cfg.metadata.name}
                    >
                      {ConfigOptionLabel({ cfg })}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button
              type="secondary"
              theme="borderless"
              size="icon"
              htmlType="button"
              aria-label="Swap left and right"
              onClick={swap}
              disabled={!data.leftName || !data.rightName}
              icon={<ArrowLeftRight className="h-5 w-5" />}
            />
            <div className="flex flex-col gap-1">
              <span className="text-xs uppercase tracking-wider text-muted-foreground">
                Right
              </span>
              <Select
                value={data.rightName ?? ""}
                onValueChange={(value) => updateUrl({ right: value })}
              >
                <SelectTrigger className="min-w-[220px]">
                  <SelectValue placeholder="Select…" />
                </SelectTrigger>
                <SelectContent>
                  {data.configs.map((cfg) => (
                    <SelectItem
                      key={cfg.metadata.name}
                      value={cfg.metadata.name}
                    >
                      {ConfigOptionLabel({ cfg })}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
        </div>
      </div>

      {!haveBoth ? (
        <EmptyContent
          title="Pick two configurations to compare."
          subtitle="Use the selectors above to choose the left and right configuration. Both must reference this service."
          size="md"
        />
      ) : sameConfig ? (
        <Alert variant="info">
          <GitCompare className="h-4 w-4" />
          <AlertTitle>These are the same configuration</AlertTitle>
          <AlertDescription>
            Pick a different configuration on either side to see a diff.
          </AlertDescription>
        </Alert>
      ) : (
        <div className="flex flex-col gap-4">
          <Alert variant="info">
            <GitCompare className="h-4 w-4" />
            <AlertTitle>{summary}</AlertTitle>
          </Alert>

          <Card>
            <CardHeader className="py-3 px-4">
              <CardTitle>Configuration metadata</CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col px-4 pb-4 pt-0">
              <div className="grid grid-cols-[180px_1fr_1fr] gap-3 text-xs uppercase tracking-wider text-muted-foreground border-b border-border/50 pb-2">
                <span>Field</span>
                <span>
                  Left ·{" "}
                  <span className="font-mono normal-case">
                    {data.left?.metadata.name}
                  </span>
                </span>
                <span>
                  Right ·{" "}
                  <span className="font-mono normal-case">
                    {data.right?.metadata.name}
                  </span>
                </span>
              </div>
              <ScalarDiffRow
                label="Version"
                equal={
                  (data.left?.spec.version ?? "") ===
                  (data.right?.spec.version ?? "")
                }
                left={data.left?.spec.version ?? ""}
                right={data.right?.spec.version ?? ""}
              />
              <ScalarDiffRow
                label="Phase"
                renderAs="badge"
                equal={data.left?.spec.phase === data.right?.spec.phase}
                left={
                  data.left ? (
                    <PhaseBadge phase={data.left.spec.phase} />
                  ) : (
                    "—"
                  )
                }
                right={
                  data.right ? (
                    <PhaseBadge phase={data.right.spec.phase} />
                  ) : (
                    "—"
                  )
                }
              />
              <ScalarDiffRow
                label="Published at"
                equal={
                  (data.left?.status?.publishedAt ?? "") ===
                  (data.right?.status?.publishedAt ?? "")
                }
                left={formatDate(data.left?.status?.publishedAt)}
                right={formatDate(data.right?.status?.publishedAt)}
              />
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="py-3 px-4">
              <CardTitle>Monitored resource types</CardTitle>
              <p className="text-xs text-muted-foreground">
                {totalChanged(mrtCounts)} change
                {totalChanged(mrtCounts) === 1 ? "" : "s"} ·{" "}
                {mrtCounts.unchanged} unchanged
              </p>
            </CardHeader>
            <CardContent className="flex flex-col gap-3 px-4 pb-4 pt-0">
              {totalChanged(mrtCounts) === 0 ? (
                <p className="text-sm text-muted-foreground">No changes.</p>
              ) : (
                mrtDiff
                  .filter((e) => e.state !== "unchanged")
                  .map((entry) => (
                    <MrtDiffEntry key={entry.key} entry={entry} />
                  ))
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="py-3 px-4">
              <CardTitle>Meters</CardTitle>
              <p className="text-xs text-muted-foreground">
                {totalChanged(meterCounts)} change
                {totalChanged(meterCounts) === 1 ? "" : "s"} ·{" "}
                {meterCounts.unchanged} unchanged
              </p>
            </CardHeader>
            <CardContent className="flex flex-col gap-3 px-4 pb-4 pt-0">
              {totalChanged(meterCounts) === 0 ? (
                <p className="text-sm text-muted-foreground">No changes.</p>
              ) : (
                meterDiff
                  .filter((e) => e.state !== "unchanged")
                  .map((entry) => (
                    <MeterDiffEntry key={entry.key} entry={entry} />
                  ))
              )}
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}
