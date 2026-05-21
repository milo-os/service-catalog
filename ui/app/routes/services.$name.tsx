import { json, redirect } from "@remix-run/node";
import type {
  ActionFunctionArgs,
  LoaderFunctionArgs,
} from "@remix-run/node";
import {
  Form,
  Link,
  useActionData,
  useLoaderData,
  useNavigation,
  useParams,
} from "@remix-run/react";
import { useEffect, useState } from "react";
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
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { Dialog } from "@datum-cloud/datum-ui/dialog";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { Input } from "@datum-cloud/datum-ui/input";
import { Label } from "@datum-cloud/datum-ui/label";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@datum-cloud/datum-ui/select";
import { Separator } from "@datum-cloud/datum-ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@datum-cloud/datum-ui/table";
import { Textarea } from "@datum-cloud/datum-ui/textarea";
import { toast } from "@datum-cloud/datum-ui/toast";
import {
  CircleAlert,
  Eye,
  GitCompare,
  Plus,
} from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import {
  conditionBadgeProps,
  formatPublishedAt,
  phaseBadgeProps,
  relativeAge,
  truncate,
} from "~/lib/format";
import type {
  KubeList,
  Phase,
  Service,
  ServiceConfiguration,
} from "~/lib/types";

interface LoaderData {
  service?: Service;
  configurations: ServiceConfiguration[];
  drafts: ServiceConfiguration[];
  published: ServiceConfiguration[];
  history: ServiceConfiguration[];
  /** The currently-active (Published) configuration, if any. */
  activeConfig?: ServiceConfiguration;
  /** Non-Retired configs that block delete. */
  configurationCount: number;
  /**
   * For each (Deprecated|Retired) config, the number of days it was active —
   * computed as the gap between its `publishedAt` and the chronologically
   * next config's `publishedAt`. Active config (no successor) is omitted.
   */
  activeDays: Record<string, number>;
  activeTab: string;
  error?: string;
}

interface ActionData {
  ok: boolean;
  intent?: string;
  error?: string;
}

const ALLOWED_TRANSITIONS: Record<Phase, Phase[]> = {
  Draft: ["Published", "Retired"],
  Published: ["Deprecated", "Retired"],
  Deprecated: ["Published", "Retired"],
  Retired: [],
};

const OWNER_PROJECT_RE =
  /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

function configSortKey(c: ServiceConfiguration): number {
  const ts = c.status?.publishedAt ?? c.metadata.creationTimestamp;
  const t = ts ? new Date(ts).getTime() : NaN;
  return Number.isNaN(t) ? 0 : t;
}

function byPublishedAtDesc(
  a: ServiceConfiguration,
  b: ServiceConfiguration
): number {
  return configSortKey(b) - configSortKey(a);
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const name = params.name;
  const activeTab =
    new URL(request.url).searchParams.get("tab") ?? "overview";
  if (!name) {
    return json(
      {
        error: "Missing service name",
        configurations: [],
        drafts: [],
        published: [],
        history: [],
        configurationCount: 0,
        activeDays: {},
        activeTab,
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
    const drafts = mine
      .filter((c) => c.spec.phase === "Draft")
      .sort(byPublishedAtDesc);
    const published = mine
      .filter((c) => c.spec.phase === "Published")
      .sort(byPublishedAtDesc);
    const history = mine
      .filter(
        (c) => c.spec.phase === "Deprecated" || c.spec.phase === "Retired"
      )
      .sort(byPublishedAtDesc);
    const configurationCount = mine.filter(
      (c) => c.spec.phase !== "Retired"
    ).length;
    // Compute "was active N days" for non-active configs.
    const withPublishedAt = mine
      .filter((c) => !!c.status?.publishedAt)
      .sort(
        (a, b) =>
          new Date(a.status!.publishedAt!).getTime() -
          new Date(b.status!.publishedAt!).getTime()
      );
    const activeDays: Record<string, number> = {};
    for (let i = 0; i < withPublishedAt.length - 1; i++) {
      const cur = withPublishedAt[i];
      const next = withPublishedAt[i + 1];
      const ms =
        new Date(next.status!.publishedAt!).getTime() -
        new Date(cur.status!.publishedAt!).getTime();
      activeDays[cur.metadata.name] = Math.max(
        0,
        Math.floor(ms / 86_400_000)
      );
    }
    return json({
      service,
      configurations: mine,
      drafts,
      published,
      history,
      activeConfig: published[0],
      configurationCount,
      activeDays,
      activeTab,
    } satisfies LoaderData);
  } catch (e) {
    return json({
      error: e instanceof Error ? e.message : String(e),
      configurations: [],
      drafts: [],
      published: [],
      history: [],
      configurationCount: 0,
      activeDays: {},
      activeTab,
    } satisfies LoaderData);
  }
}

export async function action({ request, params }: ActionFunctionArgs) {
  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");
  const name = params.name;
  if (!name) {
    return json(
      { ok: false, error: "Missing service name." } satisfies ActionData,
      { status: 400 }
    );
  }
  const apiPath = `/apis/services.miloapis.com/v1alpha1/services/${encodeURIComponent(name)}`;

  try {
    switch (intent) {
      case "updateIdentity": {
        const displayName = String(form.get("displayName") ?? "").trim();
        const description = String(form.get("description") ?? "");
        const ownerProject = String(form.get("ownerProject") ?? "").trim();
        if (displayName.length < 1 || displayName.length > 120) {
          return json(
            {
              ok: false,
              intent,
              error: "Display name must be between 1 and 120 characters.",
            } satisfies ActionData,
            { status: 400 }
          );
        }
        if (description.length > 1000) {
          return json(
            {
              ok: false,
              intent,
              error: "Description must be 1000 characters or fewer.",
            } satisfies ActionData,
            { status: 400 }
          );
        }
        if (!OWNER_PROJECT_RE.test(ownerProject)) {
          return json(
            {
              ok: false,
              intent,
              error:
                "Owner project must be a lowercase DNS-1123 name (a-z, 0-9, -).",
            } satisfies ActionData,
            { status: 400 }
          );
        }
        const patch = [
          { op: "replace", path: "/spec/displayName", value: displayName },
          { op: "replace", path: "/spec/description", value: description },
          {
            op: "replace",
            path: "/spec/owner/producerProjectRef/name",
            value: ownerProject,
          },
        ];
        await fetchK8s(request, apiPath, {
          method: "PATCH",
          headers: { "Content-Type": "application/json-patch+json" },
          body: JSON.stringify(patch),
        });
        return json({ ok: true, intent } satisfies ActionData);
      }
      case "updatePhase":
      case "deprecate": {
        const phase =
          intent === "deprecate" ? "Deprecated" : String(form.get("phase") ?? "");
        if (!["Draft", "Published", "Deprecated", "Retired"].includes(phase)) {
          return json(
            {
              ok: false,
              intent,
              error: `Unknown phase: ${phase}`,
            } satisfies ActionData,
            { status: 400 }
          );
        }
        const patch = [{ op: "replace", path: "/spec/phase", value: phase }];
        await fetchK8s(request, apiPath, {
          method: "PATCH",
          headers: { "Content-Type": "application/json-patch+json" },
          body: JSON.stringify(patch),
        });
        return json({ ok: true, intent } satisfies ActionData);
      }
      case "activateConfig": {
        const configName = String(form.get("configName") ?? "");
        if (!configName) {
          return json(
            {
              ok: false,
              intent,
              error: "Missing configuration name.",
            } satisfies ActionData,
            { status: 400 }
          );
        }
        const patch = [
          { op: "replace", path: "/spec/phase", value: "Published" },
        ];
        await fetchK8s(
          request,
          `/apis/services.miloapis.com/v1alpha1/serviceconfigurations/${encodeURIComponent(configName)}`,
          {
            method: "PATCH",
            headers: { "Content-Type": "application/json-patch+json" },
            body: JSON.stringify(patch),
          }
        );
        return json({ ok: true, intent } satisfies ActionData);
      }
      case "delete": {
        const confirm = String(form.get("confirm") ?? "");
        const serviceName = String(form.get("serviceName") ?? "");
        if (!serviceName || confirm !== serviceName) {
          return json(
            {
              ok: false,
              intent,
              error: "Confirmation did not match the service name.",
            } satisfies ActionData,
            { status: 400 }
          );
        }
        await fetchK8s(request, apiPath, { method: "DELETE" });
        return redirect("/services");
      }
      default:
        return json(
          {
            ok: false,
            intent,
            error: `Unknown intent: ${intent}`,
          } satisfies ActionData,
          { status: 400 }
        );
    }
  } catch (e) {
    return json(
      {
        ok: false,
        intent,
        error: e instanceof Error ? e.message : String(e),
      } satisfies ActionData,
      { status: 500 }
    );
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

function ConfigCard({
  cfg,
  serviceParam,
  active,
  activeDays,
}: {
  cfg: ServiceConfiguration;
  serviceParam: string;
  active: boolean;
  activeDays?: number;
}) {
  const phase = phaseBadgeProps(cfg.spec.phase);
  const navigation = useNavigation();
  const isSubmitting = navigation.state !== "idle";
  const versionLabel = cfg.spec.version
    ? `v${cfg.spec.version}`
    : cfg.metadata.name;
  const mrtCount = cfg.spec.monitoredResourceTypes?.length ?? 0;
  const meterCount = cfg.spec.meters?.length ?? 0;
  const detailHref = `/services/${encodeURIComponent(
    serviceParam
  )}/configurations/${encodeURIComponent(cfg.metadata.name)}`;
  const compareHref = `/services/${encodeURIComponent(
    serviceParam
  )}/configurations/compare?left=${encodeURIComponent(cfg.metadata.name)}`;
  const linkClass =
    "inline-flex items-center gap-1.5 h-7 px-2.5 rounded-md text-xs text-primary hover:bg-muted transition-colors";

  return (
    <div
      className={`flex items-center gap-3 px-4 py-3 rounded-lg border bg-card${
        active ? " border-2 border-success-300" : ""
      }`}
    >
      {/* Left: name + phase badge */}
      <span className="font-medium text-sm text-foreground">{versionLabel}</span>
      <Badge type={phase.type} theme={phase.theme}>
        {phase.label}
      </Badge>

      {/* Middle: meta */}
      <span className="flex-1 text-sm text-muted-foreground">
        {mrtCount} monitored resource{mrtCount === 1 ? "" : "s"}
        {" · "}
        {meterCount} meter{meterCount === 1 ? "" : "s"}
        {typeof activeDays === "number"
          ? ` · was active ${activeDays} day${activeDays === 1 ? "" : "s"}`
          : null}
      </span>

      {/* Right: actions */}
      {cfg.spec.phase === "Draft" ? (
        <Form method="post" replace>
          <input type="hidden" name="intent" value="activateConfig" />
          <input type="hidden" name="configName" value={cfg.metadata.name} />
          <button
            type="submit"
            disabled={isSubmitting}
            className={linkClass}
          >
            Activate
          </button>
        </Form>
      ) : null}
      <Link to={detailHref} className={linkClass}>
        <Eye className="h-3.5 w-3.5" />
        View details
      </Link>
      <Link to={compareHref} className={linkClass}>
        <GitCompare className="h-3.5 w-3.5" />
        Compare
      </Link>
    </div>
  );
}

function PendingBanner({
  drafts,
  serviceParam,
}: {
  drafts: ServiceConfiguration[];
  serviceParam: string;
}) {
  const navigation = useNavigation();
  const count = drafts.length;
  const first = drafts[0];
  const viewHref = `/services/${encodeURIComponent(
    serviceParam
  )}/configurations/${encodeURIComponent(first.metadata.name)}`;
  const isSubmitting = navigation.state !== "idle";
  return (
    <Alert variant="warning">
      <CircleAlert className="h-4 w-4" />
      <AlertTitle>
        {count} pending configuration{count === 1 ? "" : "s"} not yet activated
      </AlertTitle>
      <AlertDescription className="flex justify-end items-center gap-2 pt-2">
        <Link
          to={viewHref}
          className="inline-flex items-center h-9 px-3 rounded-md text-sm text-primary hover:bg-muted"
        >
          View
        </Link>
        <Form method="post" replace>
          <input type="hidden" name="intent" value="activateConfig" />
          <input
            type="hidden"
            name="configName"
            value={first.metadata.name}
          />
          <Button
            type="primary"
            theme="solid"
            size="small"
            htmlType="submit"
            disabled={isSubmitting}
            title={
              count > 1
                ? `Activates ${first.metadata.name} (newest draft). Activate other drafts from their detail pages.`
                : `Publish ${first.metadata.name}`
            }
          >
            {isSubmitting ? "Activating…" : "Activate"}
          </Button>
        </Form>
      </AlertDescription>
    </Alert>
  );
}

function OverviewTabBody({
  service,
  activeConfig,
  serviceParam,
}: {
  service: Service;
  activeConfig?: ServiceConfiguration;
  serviceParam: string;
}) {
  const phase = phaseBadgeProps(service.spec.phase);
  const conditions = service.status?.conditions ?? [];
  const mrts = activeConfig?.spec.monitoredResourceTypes ?? [];
  const meters = activeConfig?.spec.meters ?? [];
  const activeConfigHref = activeConfig
    ? `/services/${encodeURIComponent(serviceParam)}/configurations/${encodeURIComponent(activeConfig.metadata.name)}`
    : null;

  return (
    <div className="flex flex-col gap-4">
      {service.spec.phase === "Published" ? (
        <Card>
          <CardHeader>
            <CardTitle>Quick actions</CardTitle>
          </CardHeader>
          <CardContent>
            <Link
              to="/catalog"
              className="inline-flex items-center gap-2 h-9 px-3 rounded-md text-sm text-primary hover:bg-muted"
            >
              <Eye className="h-4 w-4" />
              View in Catalog
            </Link>
          </CardContent>
        </Card>
      ) : null}

      {/* Active Configuration — shown first so it is above the fold */}
      <section className="flex flex-col gap-4">
        {activeConfigHref ? (
          <Link
            to={activeConfigHref}
            className="text-xs font-semibold uppercase tracking-wider text-muted-foreground hover:text-foreground transition-colors w-fit"
          >
            Active configuration
          </Link>
        ) : (
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Active configuration
          </h3>
        )}
        {!activeConfig ? (
          <p className="text-sm text-muted-foreground">
            No active configuration
          </p>
        ) : (
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            <Card>
              <CardHeader>
                <CardTitle>Monitored Resources</CardTitle>
              </CardHeader>
              <CardContent className="p-0">
                {mrts.length === 0 ? (
                  <p className="px-4 py-3 text-sm text-muted-foreground">
                    None
                  </p>
                ) : (
                  <ul className="divide-y">
                    {mrts.map((mrt) => (
                      <li key={mrt.type} className="px-4 py-3">
                        <p className="text-sm font-medium text-foreground">
                          {mrt.displayName || mrt.type}
                        </p>
                        <p className="text-xs font-mono text-muted-foreground">
                          {mrt.type}
                        </p>
                      </li>
                    ))}
                  </ul>
                )}
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>Meters</CardTitle>
              </CardHeader>
              <CardContent className="p-0">
                {meters.length === 0 ? (
                  <p className="px-4 py-3 text-sm text-muted-foreground">
                    None
                  </p>
                ) : (
                  <ul className="divide-y">
                    {meters.map((meter) => (
                      <li key={meter.name} className="px-4 py-3">
                        <p className="text-sm font-medium text-foreground">
                          {meter.displayName || meter.name}
                        </p>
                        <p className="text-xs font-mono text-muted-foreground">
                          {meter.name}
                        </p>
                      </li>
                    ))}
                  </ul>
                )}
              </CardContent>
            </Card>
          </div>
        )}
      </section>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <Card>
          <CardHeader>
            <CardTitle>Details</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="grid grid-cols-2 gap-x-8 gap-y-3">
              <DefRow label="Service Name">
                <span className="font-mono text-xs">
                  {service.spec.serviceName}
                </span>
              </DefRow>
              <DefRow label="Display Name">
                {service.spec.displayName || "—"}
              </DefRow>
              <DefRow label="Phase">
                <Badge type={phase.type} theme={phase.theme}>
                  {service.spec.phase}
                </Badge>
              </DefRow>
              <DefRow label="Owner Project">
                {service.spec?.owner?.producerProjectRef?.name || "—"}
              </DefRow>
              <DefRow label="Published At">
                {formatPublishedAt(
                  service.status?.publishedAt,
                  service.spec.phase
                )}
              </DefRow>
              <DefRow label="Description" span={2}>
                {service.spec.description || "—"}
              </DefRow>
            </dl>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Conditions</CardTitle>
          </CardHeader>
          <CardContent>
            {conditions.length === 0 ? (
              <EmptyContent
                title="no conditions reported."
                subtitle="Conditions will appear here once the controller has reconciled this resource."
                size="sm"
                variant="minimal"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Type</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Reason</TableHead>
                    <TableHead>Message</TableHead>
                    <TableHead>Last Transition</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {conditions.map((c) => {
                    const sb = conditionBadgeProps(c.status);
                    return (
                      <TableRow key={c.type}>
                        <TableCell>{c.type}</TableCell>
                        <TableCell>
                          <Badge type={sb.type} theme={sb.theme}>
                            {c.status}
                          </Badge>
                        </TableCell>
                        <TableCell>{c.reason || "—"}</TableCell>
                        <TableCell title={c.message ?? undefined}>
                          {truncate(c.message, 80)}
                        </TableCell>
                        <TableCell>
                          {relativeAge(c.lastTransitionTime)}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function ConfigurationsTabBody({
  drafts,
  published,
  history,
  serviceParam,
  activeDays,
}: {
  drafts: ServiceConfiguration[];
  published: ServiceConfiguration[];
  history: ServiceConfiguration[];
  serviceParam: string;
  activeDays: Record<string, number>;
}) {
  const newConfigHref = `/services/${encodeURIComponent(serviceParam)}/configurations/new`;

  if (drafts.length === 0 && published.length === 0 && history.length === 0) {
    return (
      <div className="flex flex-col items-center gap-3 py-12 text-center">
        <p className="text-sm text-muted-foreground">No configurations yet.</p>
        <Link to={newConfigHref}>
          <Button
            type="primary"
            theme="solid"
            size="default"
            htmlType="button"
            icon={<Plus className="h-4 w-4" />}
          >
            New configuration
          </Button>
        </Link>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-5">
      <div className="flex justify-end">
        <Link to={newConfigHref}>
          <Button
            type="primary"
            theme="solid"
            size="default"
            htmlType="button"
            icon={<Plus className="h-4 w-4" />}
          >
            New configuration
          </Button>
        </Link>
      </div>

      {drafts.length > 0 ? (
        <PendingBanner drafts={drafts} serviceParam={serviceParam} />
      ) : null}

      <section className="flex flex-col gap-4">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
          Active configuration
        </h3>
        {published.length === 0 ? (
          <Card className="border-dashed">
            <CardContent className="py-6">
              <p className="text-sm text-muted-foreground">
                No active configuration. The latest published version was
                retired.
              </p>
            </CardContent>
          </Card>
        ) : (
          published.map((cfg) => (
            <ConfigCard
              key={cfg.metadata.name}
              cfg={cfg}
              serviceParam={serviceParam}
              active
            />
          ))
        )}
      </section>

      {history.length > 0 ? (
        <section className="flex flex-col gap-4">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Version history
          </h3>
          {history.map((cfg) => (
            <ConfigCard
              key={cfg.metadata.name}
              cfg={cfg}
              serviceParam={serviceParam}
              active={false}
              activeDays={activeDays[cfg.metadata.name]}
            />
          ))}
        </section>
      ) : null}
    </div>
  );
}

function PhaseTransitionHelp({
  from,
  to,
}: {
  from: Phase;
  to: Phase;
}) {
  if (from === to) {
    return (
      <p className="text-xs text-muted-foreground">
        Current phase. Pick another value to transition.
      </p>
    );
  }
  let copy: string;
  if (to === "Published") {
    copy =
      from === "Draft"
        ? "Publishes the service. Consumers will be able to discover it in the catalog."
        : "Re-publishes a previously deprecated service. Consumers see it again.";
  } else if (to === "Deprecated") {
    copy =
      "Marks the service as deprecated. Existing consumers continue to see it, but it is hidden from new discovery flows.";
  } else if (to === "Retired") {
    copy =
      "Retires the service. This is terminal: the service can no longer be re-published.";
  } else {
    copy = "";
  }
  return <p className="text-xs text-muted-foreground">{copy}</p>;
}

function DangerRow({
  title,
  body,
  children,
}: {
  title: string;
  body: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div className="flex flex-col gap-1 flex-1">
        <p className="text-sm font-medium text-foreground">{title}</p>
        <p className="text-sm text-muted-foreground">{body}</p>
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  );
}

function SettingsTabBody({
  service,
  configurationCount,
}: {
  service: Service;
  configurationCount: number;
}) {
  const navigation = useNavigation();
  const actionData = useActionData<typeof action>() as ActionData | undefined;
  const [description, setDescription] = useState(
    service.spec.description ?? ""
  );
  const [phase, setPhase] = useState<Phase>(service.spec.phase);
  const [confirmText, setConfirmText] = useState("");
  const [deleteOpen, setDeleteOpen] = useState(false);

  // Reset local state if the service prop changes (after a save reloads).
  useEffect(() => {
    setDescription(service.spec.description ?? "");
    setPhase(service.spec.phase);
  }, [service]);

  // Surface action results via toast — settings-tab intents only.
  // Configuration intents (e.g. activateConfig) are surfaced from the
  // top-level component so the toast fires regardless of active tab.
  useEffect(() => {
    if (!actionData) return;
    if (actionData.ok) {
      if (actionData.intent === "updateIdentity") {
        toast.success("Service identity updated.");
      } else if (actionData.intent === "updatePhase") {
        toast.success("Service phase updated.");
      } else if (actionData.intent === "deprecate") {
        toast.success("Service deprecated.");
      }
    } else if (
      actionData.error &&
      (actionData.intent === "updateIdentity" ||
        actionData.intent === "updatePhase" ||
        actionData.intent === "deprecate" ||
        actionData.intent === "delete")
    ) {
      toast.error(actionData.error);
    }
  }, [actionData]);

  const isSubmitting = navigation.state !== "idle";
  const allowedTransitions = ALLOWED_TRANSITIONS[service.spec.phase];
  const phaseSelectDisabled = service.spec.phase === "Retired";
  const allPhases: Phase[] = ["Draft", "Published", "Deprecated", "Retired"];
  const isAllowed = (target: Phase) =>
    target === service.spec.phase || allowedTransitions.includes(target);

  const phaseLabels: Record<Phase, string> = {
    Draft: "Draft — not visible to consumers",
    Published: "Published — listed in the catalog",
    Deprecated: "Deprecated — visible but discouraged",
    Retired: "Retired — frozen, no new use",
  };

  const cannotDeprecate =
    service.spec.phase === "Deprecated" ||
    service.spec.phase === "Retired";
  const cannotDelete = configurationCount > 0;
  const cannotDeleteReason = cannotDelete
    ? `Cannot delete: ${configurationCount} configuration${
        configurationCount === 1 ? "" : "s"
      } still reference this service. Delete or retire them first.`
    : "Permanently remove this service. This cannot be undone.";

  return (
    <div className="flex flex-col gap-5 max-w-3xl">
      {/* 3a. Identity */}
      <Card>
        <CardHeader>
          <CardTitle>Service identity</CardTitle>
          <CardDescription>
            Public-facing metadata for this service.
          </CardDescription>
        </CardHeader>
        <Form method="post" replace>
          <input type="hidden" name="intent" value="updateIdentity" />
          <CardContent className="flex flex-col gap-4">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="serviceName">Service name</Label>
              <Input
                id="serviceName"
                name="serviceName"
                defaultValue={service.spec.serviceName}
                disabled
              />
              <p className="text-xs text-muted-foreground">
                Immutable. Used as the canonical reference from
                configurations.
              </p>
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="displayName">Display name</Label>
              <Input
                id="displayName"
                name="displayName"
                defaultValue={service.spec.displayName ?? ""}
                required
                maxLength={120}
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="description">Description</Label>
              <Textarea
                id="description"
                name="description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                rows={4}
                maxLength={1000}
              />
              <p className="text-xs text-muted-foreground">
                {description.length} / 1000
              </p>
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="ownerProject">Owner project</Label>
              <Input
                id="ownerProject"
                name="ownerProject"
                defaultValue={
                  service.spec?.owner?.producerProjectRef?.name ?? ""
                }
                required
              />
              <p className="text-xs text-muted-foreground">
                Project that owns this service. Must reference an existing
                project.
              </p>
            </div>
          </CardContent>
          <CardFooter className="justify-end gap-2 border-t py-3">
            <Button
              type="secondary"
              theme="borderless"
              htmlType="reset"
              onClick={() => setDescription(service.spec.description ?? "")}
            >
              Reset
            </Button>
            <Button
              type="primary"
              theme="solid"
              htmlType="submit"
              disabled={isSubmitting}
            >
              {isSubmitting ? "Saving…" : "Save changes"}
            </Button>
          </CardFooter>
        </Form>
      </Card>

      {/* 3b. Lifecycle */}
      <Card>
        <CardHeader>
          <CardTitle>Lifecycle</CardTitle>
          <CardDescription>
            Controls visibility to consumers.
          </CardDescription>
        </CardHeader>
        <Form method="post" replace>
          <input type="hidden" name="intent" value="updatePhase" />
          <CardContent className="flex flex-col gap-3">
            <Select
              name="phase"
              value={phase}
              onValueChange={(value) => setPhase(value as Phase)}
              disabled={phaseSelectDisabled}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {allPhases.map((p) => (
                  <SelectItem
                    key={p}
                    value={p}
                    disabled={!isAllowed(p)}
                  >
                    {phaseLabels[p]}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <PhaseTransitionHelp from={service.spec.phase} to={phase} />
            {phaseSelectDisabled ? (
              <p className="text-xs text-muted-foreground">
                Retired is terminal; phase can no longer be changed.
              </p>
            ) : null}
          </CardContent>
          <CardFooter className="justify-end gap-2 border-t py-3">
            <Button
              type="primary"
              theme="solid"
              htmlType="submit"
              disabled={
                isSubmitting ||
                phaseSelectDisabled ||
                phase === service.spec.phase
              }
            >
              Update phase
            </Button>
          </CardFooter>
        </Form>
      </Card>

      {/* 3c. Danger zone */}
      <Card className="border-destructive">
        <CardHeader className="bg-destructive/5">
          <CardTitle className="text-destructive">Danger zone</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4 pt-4 pb-4">
          <DangerRow
            title="Deprecate service"
            body="Hide from the consumer catalog and warn existing consumers. Reversible — you can republish later."
          >
            <Form method="post" replace>
              <input type="hidden" name="intent" value="deprecate" />
              <Button
                type="danger"
                theme="solid"
                htmlType="submit"
                disabled={isSubmitting || cannotDeprecate}
                title={
                  cannotDeprecate
                    ? "Service is already Deprecated or Retired."
                    : undefined
                }
              >
                Deprecate service
              </Button>
            </Form>
          </DangerRow>

          <Separator />

          <DangerRow title="Delete service" body={cannotDeleteReason}>
            <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
              <Dialog.Trigger asChild>
                <Button
                  type="danger"
                  theme="solid"
                  disabled={cannotDelete}
                  title={cannotDelete ? cannotDeleteReason : undefined}
                >
                  Delete service
                </Button>
              </Dialog.Trigger>
              <Dialog.Content>
                <Dialog.Header
                  title={`Delete ${
                    service.spec.displayName || service.metadata.name
                  }?`}
                  description={`Type ${service.spec.serviceName} below to confirm. This action cannot be undone.`}
                  onClose={() => setDeleteOpen(false)}
                />
                <Form method="post" replace>
                  <input type="hidden" name="intent" value="delete" />
                  <input
                    type="hidden"
                    name="serviceName"
                    value={service.spec.serviceName}
                  />
                  <Dialog.Body>
                    <div className="flex flex-col gap-2 px-5">
                      <Label htmlFor="confirm">
                        Type <code>{service.spec.serviceName}</code> to confirm
                      </Label>
                      <Input
                        id="confirm"
                        name="confirm"
                        autoComplete="off"
                        value={confirmText}
                        onChange={(e) => setConfirmText(e.target.value)}
                      />
                    </div>
                  </Dialog.Body>
                  <Dialog.Footer>
                    <Button
                      type="secondary"
                      theme="outline"
                      htmlType="button"
                      onClick={() => {
                        setDeleteOpen(false);
                        setConfirmText("");
                      }}
                    >
                      Cancel
                    </Button>
                    <Button
                      type="danger"
                      theme="solid"
                      htmlType="submit"
                      disabled={
                        isSubmitting ||
                        confirmText !== service.spec.serviceName
                      }
                    >
                      Delete service
                    </Button>
                  </Dialog.Footer>
                </Form>
              </Dialog.Content>
            </Dialog>
          </DangerRow>
        </CardContent>
      </Card>
    </div>
  );
}

export default function ServiceDetail() {
  const {
    service,
    drafts,
    published,
    history,
    activeConfig,
    configurationCount,
    activeDays,
    activeTab,
    error,
  } = useLoaderData<typeof loader>() as LoaderData;
  const params = useParams();
  const actionData = useActionData<typeof action>() as ActionData | undefined;

  // Surface activateConfig results via toast (works from any active tab).
  useEffect(() => {
    if (!actionData) return;
    if (actionData.intent !== "activateConfig") return;
    if (actionData.ok) {
      toast.success("Configuration activated.");
    } else if (actionData.error) {
      toast.error(actionData.error);
    }
  }, [actionData]);

  if (error || !service) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4">
        <PageTitle
          title={params.name ?? "Service"}
          description=""
          actionsPosition="inline"
        />
        <Card>
          <CardHeader>
            <CardTitle>Failed to load data</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              {error ?? "Service not found"}
            </p>
            <a
              href={`/services/${params.name ?? ""}`}
              className="text-sm text-primary underline mt-2 inline-block"
            >
              Retry
            </a>
          </CardContent>
        </Card>
      </div>
    );
  }

  const serviceParam = params.name ?? "";

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <div className="flex flex-col gap-1">
        <PageTitle
          title={service.spec.displayName || service.metadata.name}
          description={service.spec.description ?? ""}
          actionsPosition="inline"
        />
        <p className="text-sm text-muted-foreground font-mono">
          {service.spec.serviceName}
        </p>
      </div>

      <div className={activeTab !== "overview" ? "hidden" : ""}>
        <OverviewTabBody
          service={service}
          activeConfig={activeConfig}
          serviceParam={serviceParam}
        />
      </div>
      <div className={activeTab !== "configurations" ? "hidden" : ""}>
        <ConfigurationsTabBody
          drafts={drafts}
          published={published}
          history={history}
          serviceParam={serviceParam}
          activeDays={activeDays}
        />
      </div>
      <div className={activeTab !== "settings" ? "hidden" : ""}>
        <SettingsTabBody
          service={service}
          configurationCount={configurationCount}
        />
      </div>
    </div>
  );
}
