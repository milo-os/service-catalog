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
} from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { Input } from "@datum-cloud/datum-ui/input";
import { Search, Server } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import type { KubeList, Service, ServiceConfiguration } from "~/lib/types";

interface ConfigSummary {
  mrtCount: number;
  meterCount: number;
}

interface LoaderData {
  services: Service[];
  configSummaries: Record<string, ConfigSummary>;
  error?: string;
}

export async function loader({ request }: LoaderFunctionArgs) {
  try {
    const [serviceList, configList] = await Promise.all([
      fetchK8s<KubeList<Service>>(
        request,
        "/apis/services.miloapis.com/v1alpha1/services"
      ),
      fetchK8s<KubeList<ServiceConfiguration>>(
        request,
        "/apis/services.miloapis.com/v1alpha1/serviceconfigurations"
      ),
    ]);

    const services = (serviceList.items ?? [])
      .filter((s) => s.spec.phase === "Published")
      .sort((a, b) =>
        (a.spec.displayName ?? "").localeCompare(
          b.spec.displayName ?? "",
          undefined,
          { sensitivity: "base" }
        )
      );

    const configSummaries: Record<string, ConfigSummary> = {};
    for (const config of configList.items ?? []) {
      if (config.spec.phase !== "Published") continue;
      const serviceName = config.spec.serviceRef.name;
      configSummaries[serviceName] = {
        mrtCount: config.spec.monitoredResourceTypes?.length ?? 0,
        meterCount: config.spec.meters?.length ?? 0,
      };
    }

    return json({ services, configSummaries } satisfies LoaderData);
  } catch (e) {
    return json({
      services: [],
      configSummaries: {},
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

function matchesQuery(service: Service, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  const haystacks = [
    service.spec.displayName ?? "",
    service.spec.serviceName ?? "",
    service.spec.description ?? "",
  ];
  return haystacks.some((h) => h.toLowerCase().includes(needle));
}

function ServiceCard({
  service,
  configSummary,
}: {
  service: Service;
  configSummary?: ConfigSummary;
}) {
  const ownerProject =
    service.spec?.owner?.producerProjectRef?.name ?? "Unknown";

  const statLine =
    configSummary != null
      ? [
          configSummary.mrtCount > 0
            ? `${configSummary.mrtCount} monitored resource${configSummary.mrtCount !== 1 ? "s" : ""}`
            : null,
          configSummary.meterCount > 0
            ? `${configSummary.meterCount} meter${configSummary.meterCount !== 1 ? "s" : ""}`
            : null,
        ]
          .filter(Boolean)
          .join(" · ")
      : null;

  return (
    <li>
      <Link
        to={`/services/${encodeURIComponent(service.metadata.name)}`}
        className="block rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        <Card className="h-full transition-shadow hover:shadow-md">
          <CardHeader className="flex flex-row items-start justify-between gap-3 py-3 px-4">
            <div className="rounded-md bg-primary/10 p-2 text-primary">
              <Server className="h-5 w-5" />
            </div>
            <Badge type="success" theme="light">
              Published
            </Badge>
          </CardHeader>
          <CardContent className="flex flex-col gap-2 px-4 pb-4 pt-0">
            <h3 className="text-base font-semibold text-foreground">
              {service.spec.displayName || service.metadata.name}
            </h3>
            <p className="text-sm text-muted-foreground">
              by{" "}
              <span className="font-mono text-xs">{ownerProject}</span>
            </p>
            <p className="text-sm text-muted-foreground line-clamp-3">
              {service.spec.description || "No description provided."}
            </p>
            {statLine ? (
              <p className="text-xs text-muted-foreground">{statLine}</p>
            ) : null}
          </CardContent>
        </Card>
      </Link>
    </li>
  );
}

export default function CatalogIndex() {
  const { services, configSummaries, error } = useLoaderData<typeof loader>() as LoaderData;
  const [query, setQuery] = useState("");

  const filtered = useMemo(
    () => services.filter((s) => matchesQuery(s, query.trim())),
    [services, query]
  );

  return (
    <div className="flex flex-col gap-6 px-6 py-4 max-w-7xl">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-bold tracking-tight">Service catalog</h1>
        <p className="text-sm text-muted-foreground">
          Browse services published for your projects.
        </p>
      </header>

      {error ? (
        <Card>
          <CardHeader>
            <h2 className="text-base font-semibold">Failed to load catalog</h2>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">{error}</p>
          </CardContent>
        </Card>
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

          {services.length === 0 ? (
            <EmptyContent
              title="No services available yet."
              subtitle="Services appear here once a provider publishes them."
              size="lg"
            />
          ) : filtered.length === 0 ? (
            <Card>
              <CardContent className="flex flex-col items-center gap-3 py-8">
                <p className="text-sm font-medium">
                  No matches for &ldquo;{query}&rdquo;.
                </p>
                <p className="text-sm text-muted-foreground">
                  Try a different search or clear the filter.
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
            <ul className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
              {filtered.map((s) => (
                <ServiceCard
                  key={s.metadata.name}
                  service={s}
                  configSummary={configSummaries[s.metadata.name]}
                />
              ))}
            </ul>
          )}
        </>
      )}
    </div>
  );
}
