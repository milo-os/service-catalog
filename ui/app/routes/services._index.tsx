import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { Link, useLoaderData } from "@remix-run/react";
import { useMemo, useState } from "react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { Input } from "@datum-cloud/datum-ui/input";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import { Plus, Search } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@datum-cloud/datum-ui/table";
import { fetchK8s } from "~/lib/k8s.server";
import { phaseBadgeProps, relativeAge } from "~/lib/format";
import type {
  KubeList,
  Phase,
  Service,
  ServiceConfiguration,
} from "~/lib/types";

interface LoaderData {
  services: Service[];
  configsByService: Record<string, number>;
  configsError: boolean;
  error?: string;
}

export async function loader({ request }: LoaderFunctionArgs) {
  try {
    const [servicesResult, configsResult] = await Promise.allSettled([
      fetchK8s<KubeList<Service>>(
        request,
        "/apis/services.miloapis.com/v1alpha1/services"
      ),
      fetchK8s<KubeList<ServiceConfiguration>>(
        request,
        "/apis/services.miloapis.com/v1alpha1/serviceconfigurations"
      ),
    ]);

    if (servicesResult.status === "rejected") {
      return json({
        services: [],
        configsByService: {},
        configsError: true,
        error:
          servicesResult.reason instanceof Error
            ? servicesResult.reason.message
            : String(servicesResult.reason),
      } satisfies LoaderData);
    }

    const services = servicesResult.value.items ?? [];
    const configsError = configsResult.status === "rejected";
    const configsByService: Record<string, number> = {};
    if (configsResult.status === "fulfilled") {
      for (const cfg of configsResult.value.items ?? []) {
        const ref = cfg.spec?.serviceRef?.name;
        if (!ref) continue;
        configsByService[ref] = (configsByService[ref] ?? 0) + 1;
      }
    }

    return json({
      services,
      configsByService,
      configsError,
    } satisfies LoaderData);
  } catch (e) {
    return json({
      services: [],
      configsByService: {},
      configsError: true,
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

const PHASE_ORDER: Phase[] = ["Published", "Draft", "Deprecated", "Retired"];

function matchesQuery(service: Service, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  return [
    service.spec.displayName ?? "",
    service.spec.serviceName ?? "",
    service.spec.description ?? "",
    service.metadata.name ?? "",
  ].some((h) => h.toLowerCase().includes(needle));
}

export default function ServicesIndex() {
  const { services, configsByService, configsError, error } =
    useLoaderData<typeof loader>() as LoaderData;

  const [query, setQuery] = useState("");

  const filtered = useMemo(
    () => services.filter((s) => matchesQuery(s, query.trim())),
    [services, query]
  );

  const phaseCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const s of services) {
      counts[s.spec.phase] = (counts[s.spec.phase] ?? 0) + 1;
    }
    return counts;
  }, [services]);

  const summaryParts: string[] = [`${services.length} total`];
  for (const phase of PHASE_ORDER) {
    if (phaseCounts[phase]) {
      summaryParts.push(`${phaseCounts[phase]} ${phase}`);
    }
  }
  const summaryLine = summaryParts.join(" · ");

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <div className="flex items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <PageTitle
            title="Services"
            description="Cluster-scoped governance catalog entries for provider-registered services."
            actionsPosition="inline"
          />
          {services.length > 0 ? (
            <p className="text-sm text-muted-foreground">{summaryLine}</p>
          ) : null}
        </div>
        <Link to="/services/new" className="shrink-0">
          <Button
            type="primary"
            theme="solid"
            htmlType="button"
            icon={<Plus className="h-4 w-4" />}
          >
            New service
          </Button>
        </Link>
      </div>

      {error ? (
        <Card>
          <CardHeader>
            <CardTitle>Failed to load data</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">{error}</p>
            <a
              href="/services"
              className="text-sm text-primary underline mt-2 inline-block"
            >
              Retry
            </a>
          </CardContent>
        </Card>
      ) : services.length === 0 ? (
        <EmptyContent
          title="no services have been registered yet."
          subtitle="Services define the canonical catalog entries for provider APIs."
          size="lg"
        />
      ) : (
        <>
          <div className="relative max-w-sm">
            <Search className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="search"
              placeholder="Search services…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="pl-9"
            />
          </div>
          {filtered.length === 0 ? (
            <Card>
              <CardContent className="flex flex-col items-center gap-3 py-8">
                <p className="text-sm font-medium">
                  No matches for &ldquo;{query}&rdquo;.
                </p>
                <Button
                  type="secondary"
                  theme="outline"
                  htmlType="button"
                  onClick={() => setQuery("")}
                >
                  Clear search
                </Button>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardContent className="p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-[25%]">Name</TableHead>
                      <TableHead className="w-[20%]">Service Name</TableHead>
                      <TableHead className="w-[12%]">Phase</TableHead>
                      <TableHead className="w-[12%]">Configurations</TableHead>
                      <TableHead className="w-[16%]">Age</TableHead>
                      <TableHead className="w-[15%]">Owner</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filtered.map((s) => {
                      const phase = phaseBadgeProps(s.spec.phase);
                      const cfgCount =
                        configsByService[s.spec.serviceName] ?? 0;
                      const owner =
                        s.spec?.owner?.producerProjectRef?.name || "—";
                      return (
                        <TableRow key={s.metadata.name}>
                          <TableCell>
                            <Link
                              to={`/services/${encodeURIComponent(s.metadata.name)}`}
                              className="text-primary hover:underline"
                            >
                              {s.metadata.name}
                            </Link>
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {s.spec.serviceName}
                          </TableCell>
                          <TableCell>
                            <Badge type={phase.type} theme={phase.theme}>
                              {s.spec.phase}
                            </Badge>
                          </TableCell>
                          <TableCell>
                            {configsError ? "—" : cfgCount}
                          </TableCell>
                          <TableCell>
                            {relativeAge(s.metadata.creationTimestamp)}
                          </TableCell>
                          <TableCell>{owner}</TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
