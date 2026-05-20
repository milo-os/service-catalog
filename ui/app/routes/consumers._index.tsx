import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { Link, useLoaderData } from "@remix-run/react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@datum-cloud/datum-ui/card";
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
import { AlertCircle } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import { consumerPhaseBadgeProps, relativeAge } from "~/lib/format";
import type { KubeList, ServiceConsumer } from "~/lib/types";

interface LoaderData {
  consumers: ServiceConsumer[];
  error?: string;
}

const PHASE_SORT_ORDER: Record<string, number> = {
  PendingApproval: 0,
  Active: 1,
  Denied: 2,
};

function phaseOrder(phase: string | undefined): number {
  return PHASE_SORT_ORDER[phase ?? ""] ?? 99;
}

export async function loader({ request }: LoaderFunctionArgs) {
  try {
    const list = await fetchK8s<KubeList<ServiceConsumer>>(
      request,
      "/apis/services.miloapis.com/v1alpha1/serviceconsumers"
    );

    const consumers = (list.items ?? []).sort(
      (a, b) =>
        phaseOrder(a.status?.phase) - phaseOrder(b.status?.phase)
    );

    return json({ consumers } satisfies LoaderData);
  } catch (e) {
    return json({
      consumers: [],
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

export default function ConsumersIndex() {
  const { consumers, error } = useLoaderData<typeof loader>() as LoaderData;

  const pendingCount = consumers.filter(
    (c) => c.status?.phase === "PendingApproval"
  ).length;
  const activeCount = consumers.filter(
    (c) => c.status?.phase === "Active"
  ).length;
  const deniedCount = consumers.filter(
    (c) => c.status?.phase === "Denied"
  ).length;

  const summaryParts: string[] = [];
  if (pendingCount > 0) summaryParts.push(`${pendingCount} pending`);
  if (activeCount > 0) summaryParts.push(`${activeCount} active`);
  if (deniedCount > 0) summaryParts.push(`${deniedCount} denied`);
  const summaryLine = summaryParts.join(" · ");

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <div className="flex flex-col gap-1">
        <PageTitle
          title="Access requests"
          description="Review and approve consumer requests for your private beta services."
          actionsPosition="inline"
        />
        {consumers.length > 0 && summaryLine ? (
          <p className="text-sm text-muted-foreground">{summaryLine}</p>
        ) : null}
      </div>

      {error ? (
        <Card>
          <CardHeader>
            <CardTitle>Failed to load data</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">{error}</p>
            <a
              href="/consumers"
              className="text-sm text-primary underline mt-2 inline-block"
            >
              Retry
            </a>
          </CardContent>
        </Card>
      ) : consumers.length === 0 ? (
        <EmptyContent
          title="No access requests yet."
          subtitle="When consumers request access to your gated services, they'll appear here."
          size="lg"
        />
      ) : (
        <>
          {pendingCount > 0 ? (
            <div className="flex items-start gap-3 rounded-lg border border-warning-300 bg-warning-50 px-4 py-3 text-sm text-warning-800">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-warning-500" />
              <p>
                You have{" "}
                <span className="font-semibold">{pendingCount}</span> pending
                request{pendingCount === 1 ? "" : "s"} to review.
              </p>
            </div>
          ) : null}

          <Card>
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-[28%]">Consumer project</TableHead>
                    <TableHead className="w-[25%]">Service</TableHead>
                    <TableHead className="w-[14%]">Status</TableHead>
                    <TableHead className="w-[14%]">Requested</TableHead>
                    <TableHead className="w-[19%]">Action</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {consumers.map((consumer) => {
                    const phase = consumer.status?.phase;
                    const badge = consumerPhaseBadgeProps(phase ?? "");
                    return (
                      <TableRow key={consumer.metadata.name}>
                        <TableCell className="font-mono text-xs">
                          {consumer.spec.consumerProjectRef.name}
                        </TableCell>
                        <TableCell>
                          <Link
                            to={`/services/${encodeURIComponent(consumer.spec.serviceRef.name)}`}
                            className="text-primary hover:underline"
                          >
                            {consumer.spec.serviceRef.name}
                          </Link>
                        </TableCell>
                        <TableCell>
                          <Badge type={badge.type} theme={badge.theme}>
                            {badge.label}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          {relativeAge(consumer.metadata.creationTimestamp)}
                        </TableCell>
                        <TableCell>
                          {phase === "PendingApproval" ? (
                            <Link
                              to={`/consumers/${encodeURIComponent(consumer.metadata.name)}`}
                              className="text-sm text-primary hover:underline"
                            >
                              Review
                            </Link>
                          ) : (
                            <span className="text-sm text-muted-foreground">
                              {badge.label}
                            </span>
                          )}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}
