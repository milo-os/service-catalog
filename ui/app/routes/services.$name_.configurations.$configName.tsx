import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { Link, useLoaderData, useParams } from "@remix-run/react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import { fetchK8s } from "~/lib/k8s.server";
import { formatUnit, phaseBadgeProps } from "~/lib/format";
import type {
  MeterSpec,
  MonitoredResourceTypeSpec,
  ServiceConfiguration,
} from "~/lib/types";

interface LoaderData {
  configuration?: ServiceConfiguration;
  error?: string;
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const configName = params.configName;
  if (!configName) {
    return json(
      { error: "Missing configuration name" } satisfies LoaderData,
      { status: 400 }
    );
  }
  try {
    const configuration = await fetchK8s<ServiceConfiguration>(
      request,
      `/apis/services.miloapis.com/v1alpha1/serviceconfigurations/${encodeURIComponent(configName)}`
    );
    return json({ configuration } satisfies LoaderData);
  } catch (e) {
    return json({
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

function DefRow({
  label,
  children,
  span = 1,
}: {
  label: string;
  children: React.ReactNode;
  span?: 1 | 2;
}) {
  return (
    <div className={span === 2 ? "col-span-2" : undefined}>
      <dt className="text-sm font-medium text-muted-foreground">{label}</dt>
      <dd className="text-sm text-foreground mt-1 whitespace-pre-line">
        {children}
      </dd>
    </div>
  );
}

function MeterCard({ meter }: { meter: MeterSpec }) {
  return (
    <div className="border border-border/50 rounded-md p-3 flex flex-col gap-2">
      <div className="flex flex-col gap-1">
        <div className="font-mono text-xs text-foreground">{meter.name}</div>
        {meter.displayName ? (
          <div className="text-sm font-medium text-foreground">
            {meter.displayName}
          </div>
        ) : null}
        {meter.description ? (
          <div className="text-sm text-muted-foreground whitespace-pre-line">
            {meter.description}
          </div>
        ) : null}
      </div>
      <dl className="grid grid-cols-2 md:grid-cols-4 gap-x-4 gap-y-2">
        <DefRow label="Aggregation">
          {meter.measurement?.aggregation || "—"}
        </DefRow>
        <DefRow label="Unit">
          {formatUnit(meter.measurement?.unit, meter.measurement?.unitDisplayName)}
        </DefRow>
        <DefRow label="Consumed Unit">
          {formatUnit(meter.billing?.consumedUnit, meter.billing?.consumedUnitDisplayName)}
        </DefRow>
        <DefRow label="Pricing Unit">
          {formatUnit(meter.billing?.pricingUnit, meter.billing?.pricingUnitDisplayName)}
        </DefRow>
      </dl>
      {meter.monitoredResourceTypes &&
      meter.monitoredResourceTypes.length > 0 ? (
        <div className="pt-1">
          <div className="text-xs font-medium text-muted-foreground mb-1">
            Monitored Resources
          </div>
          <div className="flex flex-wrap gap-1.5">
            {meter.monitoredResourceTypes.map((mrt) => (
              <Badge key={mrt} type="secondary" theme="light">
                {mrt}
              </Badge>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function MrtCard({ mrt }: { mrt: MonitoredResourceTypeSpec }) {
  return (
    <div className="border border-border/50 rounded-md p-3 flex flex-col gap-2">
      <div className="flex flex-col gap-1">
        <div className="font-mono text-xs text-foreground">{mrt.type}</div>
        {mrt.displayName ? (
          <div className="text-sm font-medium text-foreground">
            {mrt.displayName}
          </div>
        ) : null}
        {mrt.description ? (
          <div className="text-sm text-muted-foreground whitespace-pre-line">
            {mrt.description}
          </div>
        ) : null}
      </div>
      <div className="text-xs text-muted-foreground font-mono">
        <span>{mrt.gvk?.group || "—"}</span>
        <span className="mx-1.5">/</span>
        <span>{mrt.gvk?.kind || "—"}</span>
      </div>
      {mrt.labels && mrt.labels.length > 0 ? (
        <div>
          <div className="text-xs font-medium text-muted-foreground mb-1">
            Labels
          </div>
          <div className="flex flex-wrap gap-1.5">
            {mrt.labels.map((l) => (
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
        </div>
      ) : null}
    </div>
  );
}

export default function ServiceConfigurationDetail() {
  const { configuration, error } = useLoaderData<typeof loader>() as LoaderData;
  const params = useParams();
  const serviceName = params.name ?? "";
  const backHref = `/services/${encodeURIComponent(serviceName)}?tab=configurations`;

  if (error || !configuration) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4">
        <Link to={backHref} className="text-sm text-primary hover:underline">
          ← Back
        </Link>
        <PageTitle
          title={params.configName ?? "Configuration"}
          description=""
          actionsPosition="inline"
        />
        <Card>
          <CardHeader>
            <CardTitle>Failed to load configuration</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              {error ?? "Configuration not found"}
            </p>
          </CardContent>
        </Card>
      </div>
    );
  }

  const phase = phaseBadgeProps(configuration.spec.phase);
  const meters = configuration.spec.meters ?? [];
  const mrts = configuration.spec.monitoredResourceTypes ?? [];
  const serviceRefName = configuration.spec.serviceRef?.name;

  const versionTitle = configuration.spec.version
    ? `v${configuration.spec.version}`
    : configuration.metadata.name;

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <Link to={backHref} className="text-sm text-primary hover:underline">
        ← Back
      </Link>
      <div className="flex flex-col gap-1">
        <div className="flex items-center gap-3 flex-wrap">
          <h1 className="text-2xl font-bold text-foreground">
            {versionTitle}
          </h1>
          <Badge type={phase.type} theme={phase.theme}>
            {phase.label}
          </Badge>
        </div>
        <div className="flex items-center gap-x-4 gap-y-1 flex-wrap text-xs text-muted-foreground">
          <span className="font-mono">{configuration.metadata.name}</span>
          {serviceRefName ? (
            <>
              <span aria-hidden>·</span>
              <span>
                Service:&nbsp;
                <Link
                  to={`/services/${encodeURIComponent(serviceRefName)}`}
                  className="text-primary hover:underline font-mono"
                >
                  {serviceRefName}
                </Link>
              </span>
            </>
          ) : null}
          <span aria-hidden>·</span>
          <span>
            {meters.length} meter{meters.length === 1 ? "" : "s"}
          </span>
          <span aria-hidden>·</span>
          <span>
            {mrts.length} resource type{mrts.length === 1 ? "" : "s"}
          </span>
        </div>
      </div>

      <Card>
        <CardHeader className="py-3 px-4 flex-row items-center justify-between">
          <CardTitle className="text-base">Meters</CardTitle>
          <span className="text-xs text-muted-foreground">
            {meters.length} total
          </span>
        </CardHeader>
        <CardContent className="px-4 pb-4 pt-0">
          {meters.length === 0 ? (
            <EmptyContent
              title="no meters defined."
              subtitle="Meters declared by this configuration will appear here."
              size="sm"
              variant="minimal"
            />
          ) : (
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
              {meters.map((m) => (
                <MeterCard key={m.name} meter={m} />
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="py-3 px-4 flex-row items-center justify-between">
          <CardTitle className="text-base">Monitored Resources</CardTitle>
          <span className="text-xs text-muted-foreground">
            {mrts.length} total
          </span>
        </CardHeader>
        <CardContent className="px-4 pb-4 pt-0">
          {mrts.length === 0 ? (
            <EmptyContent
              title="no monitored resources defined."
              subtitle="Monitored resources declared by this configuration will appear here."
              size="sm"
              variant="minimal"
            />
          ) : (
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
              {mrts.map((mrt) => (
                <MrtCard key={mrt.type} mrt={mrt} />
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
