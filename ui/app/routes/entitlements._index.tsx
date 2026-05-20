import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { Link, useLoaderData } from "@remix-run/react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
} from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@datum-cloud/datum-ui/table";
import { Plus } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import { entitlementPhaseBadgeProps, relativeAge, formatDate } from "~/lib/format";
import type { KubeList, ServiceEntitlement } from "~/lib/types";

const PHASE_ORDER = ["Active", "PendingApproval", "Rejected"] as const;

interface LoaderData {
  entitlements: ServiceEntitlement[];
  error?: string;
}

export async function loader({ request }: LoaderFunctionArgs) {
  try {
    const list = await fetchK8s<KubeList<ServiceEntitlement>>(
      request,
      "/apis/services.miloapis.com/v1alpha1/serviceentitlements"
    );

    const phaseRank = (e: ServiceEntitlement): number => {
      const phase = e.status?.phase ?? "";
      const idx = PHASE_ORDER.indexOf(phase as typeof PHASE_ORDER[number]);
      return idx === -1 ? PHASE_ORDER.length : idx;
    };

    const entitlements = (list.items ?? []).sort(
      (a, b) => phaseRank(a) - phaseRank(b)
    );

    return json({ entitlements } satisfies LoaderData);
  } catch (e) {
    return json({
      entitlements: [],
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

export default function EntitlementsIndex() {
  const { entitlements, error } = useLoaderData<typeof loader>() as LoaderData;

  const total = entitlements.length;
  const activeCount = entitlements.filter(
    (e) => e.status?.phase === "Active"
  ).length;
  const pendingCount = entitlements.filter(
    (e) => e.status?.phase === "PendingApproval"
  ).length;

  const summaryLine =
    total > 0
      ? [
          `${total} total`,
          activeCount > 0 ? `${activeCount} Active` : null,
          pendingCount > 0 ? `${pendingCount} Pending` : null,
        ]
          .filter(Boolean)
          .join(" · ")
      : null;

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <div className="flex items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <PageTitle
            title="My service access"
            description="Services you have enabled or requested access to."
            actionsPosition="inline"
          />
          {summaryLine ? (
            <p className="text-sm text-muted-foreground">{summaryLine}</p>
          ) : null}
        </div>
        <Link to="/catalog" className="shrink-0">
          <Button
            type="primary"
            theme="solid"
            htmlType="button"
            icon={<Plus className="h-4 w-4" />}
          >
            Browse catalog
          </Button>
        </Link>
      </div>

      {error ? (
        <Card>
          <CardHeader>
            <h2 className="text-base font-semibold">Failed to load entitlements</h2>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">{error}</p>
            <a
              href="/entitlements"
              className="text-sm text-primary underline mt-2 inline-block"
            >
              Retry
            </a>
          </CardContent>
        </Card>
      ) : entitlements.length === 0 ? (
        <EmptyContent
          title="No service access yet."
          subtitle="Browse the catalog to enable services for your project."
          size="lg"
          actions={
            <Link to="/catalog">
              <Button type="primary" theme="solid" htmlType="button">
                Browse catalog
              </Button>
            </Link>
          }
        />
      ) : (
        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[25%]">Service</TableHead>
                  <TableHead className="w-[15%]">Status</TableHead>
                  <TableHead className="w-[20%]">Origin</TableHead>
                  <TableHead className="w-[15%]">Requested</TableHead>
                  <TableHead className="w-[25%]">Active Since</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {entitlements.map((e) => {
                  const serviceRef = e.spec.serviceRef.name;
                  const phase = e.status?.phase;
                  const origin = e.status?.origin;
                  const badgeProps = entitlementPhaseBadgeProps(phase ?? "");
                  const isDependency = origin === "Dependency";

                  return (
                    <TableRow key={e.metadata.name}>
                      <TableCell>
                        <Link
                          to={`/catalog/${encodeURIComponent(serviceRef)}`}
                          className="text-primary hover:underline"
                        >
                          {serviceRef}
                        </Link>
                      </TableCell>
                      <TableCell>
                        {phase ? (
                          <Badge type={badgeProps.type} theme={badgeProps.theme}>
                            {badgeProps.label}
                          </Badge>
                        ) : (
                          <span className="text-muted-foreground text-sm">—</span>
                        )}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col gap-0.5">
                          <div className="flex items-center gap-1.5">
                            <span className="text-sm">
                              {isDependency ? "Dependency" : "Direct"}
                            </span>
                            {isDependency ? (
                              <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground font-medium">
                                auto
                              </span>
                            ) : null}
                          </div>
                          {isDependency && e.status?.dependencyOf ? (
                            <span className="text-xs text-muted-foreground">
                              via {e.status.dependencyOf}
                            </span>
                          ) : null}
                        </div>
                      </TableCell>
                      <TableCell>
                        {relativeAge(e.metadata.creationTimestamp)}
                      </TableCell>
                      <TableCell>
                        {phase === "Active"
                          ? formatDate(e.status?.entitledAt)
                          : "—"}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
