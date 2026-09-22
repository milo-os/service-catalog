import { json, redirect } from "@remix-run/node";
import type { ActionFunctionArgs, LoaderFunctionArgs } from "@remix-run/node";
import { Form, Link, useActionData, useLoaderData, useNavigation } from "@remix-run/react";
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
import { Label } from "@datum-cloud/datum-ui/label";
import { Textarea } from "@datum-cloud/datum-ui/textarea";
import { Alert, AlertDescription, AlertTitle } from "@datum-cloud/datum-ui/alert";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import { ArrowLeft, Clock, CheckCircle2, XCircle, Info } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import {
  enablementModeBadgeProps,
  entitlementPhaseBadgeProps,
  formatDate,
} from "~/lib/format";
import type { KubeList, Service, ServiceEntitlement } from "~/lib/types";

interface LoaderData {
  service?: Service;
  entitlement?: ServiceEntitlement;
  error?: string;
}

interface ActionData {
  ok: boolean;
  error?: string;
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const name = params.name;
  if (!name) {
    return json(
      { error: "Missing service name." } satisfies LoaderData,
      { status: 400 }
    );
  }

  try {
    const [service, entitlementList] = await Promise.all([
      fetchK8s<Service>(
        request,
        `/apis/services.miloapis.com/v1alpha1/services/${encodeURIComponent(name)}`
      ),
      fetchK8s<KubeList<ServiceEntitlement>>(
        request,
        "/apis/services.miloapis.com/v1alpha1/serviceentitlements"
      ),
    ]);

    const entitlement = (entitlementList.items ?? []).find(
      (e) => e.spec.serviceRef.name === name
    );

    return json({ service, entitlement } satisfies LoaderData);
  } catch (e) {
    return json(
      {
        error: e instanceof Error ? e.message : String(e),
      } satisfies LoaderData,
      { status: 500 }
    );
  }
}

export async function action({ request, params }: ActionFunctionArgs) {
  const name = params.name;
  if (!name) {
    return json(
      { ok: false, error: "Missing service name." } satisfies ActionData,
      { status: 400 }
    );
  }

  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");
  const requestMessage = String(form.get("requestMessage") ?? "").trim();

  if (intent !== "enable" && intent !== "request-access") {
    return json(
      { ok: false, error: `Unknown intent: ${intent}` } satisfies ActionData,
      { status: 400 }
    );
  }

  try {
    await fetchK8s<ServiceEntitlement>(
      request,
      "/apis/services.miloapis.com/v1alpha1/serviceentitlements",
      {
        method: "POST",
        body: JSON.stringify({
          apiVersion: "services.miloapis.com/v1alpha1",
          kind: "ServiceEntitlement",
          metadata: { name: `entitlement-${name}-${Date.now()}` },
          spec: {
            serviceRef: { name },
            requestMessage:
              intent === "request-access" && requestMessage
                ? requestMessage
                : undefined,
          },
        }),
      }
    );
    return redirect("/entitlements");
  } catch (e) {
    return json(
      {
        ok: false,
        error: e instanceof Error ? e.message : String(e),
      } satisfies ActionData,
      { status: 500 }
    );
  }
}

export default function CatalogDetail() {
  const { service, entitlement, error } = useLoaderData<typeof loader>() as LoaderData;
  const actionData = useActionData<typeof action>() as ActionData | undefined;
  const navigation = useNavigation();
  const isSubmitting = navigation.state !== "idle";

  if (error || !service) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4 max-w-3xl">
        <Link
          to="/catalog"
          className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors w-fit"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to catalog
        </Link>
        <Card>
          <CardHeader>
            <CardTitle>Service not found</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              {error ?? "This service could not be loaded. It may not exist or you may not have access to it."}
            </p>
          </CardContent>
        </Card>
      </div>
    );
  }

  const displayName = service.spec.displayName || service.metadata.name;
  const ownerProject = service.spec.owner?.producerProjectRef?.name ?? "Unknown";
  const enablementMode = service.spec.enablementPolicy?.mode ?? "SelfService";
  const isGated = enablementMode === "GatedByProvider";
  const modeBadge = enablementModeBadgeProps(enablementMode);
  const dependencies = service.spec.dependencies ?? [];

  const entitlementPhase = entitlement?.status?.phase;
  const hasActiveEntitlement = entitlementPhase === "Active";
  const isPending = entitlementPhase === "PendingApproval";
  const isRejected = entitlementPhase === "Rejected";

  // Show the action panel if there's no entitlement at all, or if rejected
  const showActionPanel = !entitlement || isRejected;

  return (
    <div className="flex flex-col gap-6 px-6 py-4 max-w-3xl">
      {/* Back link */}
      <Link
        to="/catalog"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors w-fit"
      >
        <ArrowLeft className="h-4 w-4" />
        Back to catalog
      </Link>

      {/* Header */}
      <header className="flex flex-col gap-2">
        <div className="flex items-center gap-3 flex-wrap">
          <PageTitle
            title={displayName}
            description=""
            actionsPosition="inline"
          />
          <Badge type={modeBadge.type} theme={modeBadge.theme}>
            {modeBadge.label}
          </Badge>
        </div>
        {service.spec.description ? (
          <p className="text-sm text-muted-foreground leading-relaxed">
            {service.spec.description}
          </p>
        ) : null}
        <p className="text-xs text-muted-foreground">
          Provided by{" "}
          <span className="font-mono">{ownerProject}</span>
        </p>
      </header>

      {/* Action error banner */}
      {actionData && !actionData.ok && actionData.error ? (
        <Alert variant="destructive">
          <XCircle className="h-4 w-4" />
          <AlertTitle>Request failed</AlertTitle>
          <AlertDescription>{actionData.error}</AlertDescription>
        </Alert>
      ) : null}

      {/* Current entitlement status */}
      {entitlement ? (
        <section className="flex flex-col gap-3">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Your access
          </h2>

          {hasActiveEntitlement ? (
            <Card>
              <CardContent className="flex items-center gap-3 py-4">
                <CheckCircle2 className="h-5 w-5 text-success shrink-0" />
                <div className="flex flex-col gap-0.5">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium text-foreground">
                      Access granted
                    </span>
                    {(() => {
                      const ep = entitlementPhaseBadgeProps("Active");
                      return (
                        <Badge type={ep.type} theme={ep.theme}>
                          {ep.label}
                        </Badge>
                      );
                    })()}
                  </div>
                  {entitlement.status?.entitledAt ? (
                    <p className="text-xs text-muted-foreground">
                      Entitled since{" "}
                      {formatDate(entitlement.status.entitledAt)}
                    </p>
                  ) : null}
                </div>
              </CardContent>
            </Card>
          ) : null}

          {isPending ? (
            <Alert variant="info">
              <Clock className="h-4 w-4" />
              <AlertTitle>Your request is under review</AlertTitle>
              <AlertDescription>
                The service provider will review your request and get back to
                you. You'll be notified when a decision is made.
              </AlertDescription>
            </Alert>
          ) : null}

          {isRejected ? (
            <Card>
              <CardContent className="flex items-center gap-3 py-4">
                <XCircle className="h-5 w-5 text-destructive shrink-0" />
                <div className="flex flex-col gap-0.5">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium text-foreground">
                      Access request was not approved
                    </span>
                    {(() => {
                      const ep = entitlementPhaseBadgeProps("Rejected");
                      return (
                        <Badge type={ep.type} theme={ep.theme}>
                          {ep.label}
                        </Badge>
                      );
                    })()}
                  </div>
                  <p className="text-xs text-muted-foreground">
                    You can submit a new request below.
                  </p>
                </div>
              </CardContent>
            </Card>
          ) : null}
        </section>
      ) : null}

      {/* Enable / Request access panel */}
      {showActionPanel ? (
        <section className="flex flex-col gap-3">
          {isGated ? (
            <Card>
              <CardHeader>
                <CardTitle>Request access</CardTitle>
                <CardDescription>
                  This service is in private beta. Submit a request and the
                  provider will review your application.
                </CardDescription>
              </CardHeader>
              <Form method="post" replace>
                <input type="hidden" name="intent" value="request-access" />
                <CardContent className="flex flex-col gap-3">
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="requestMessage">
                      Message{" "}
                      <span className="text-xs text-muted-foreground font-normal">
                        (optional)
                      </span>
                    </Label>
                    <Textarea
                      id="requestMessage"
                      name="requestMessage"
                      rows={4}
                      placeholder="Describe how you plan to use this service (optional)"
                    />
                    <p className="text-xs text-muted-foreground">
                      A message can help the provider evaluate your request
                      faster.
                    </p>
                  </div>
                </CardContent>
                <CardFooter className="border-t py-3">
                  <Button
                    type="primary"
                    theme="solid"
                    htmlType="submit"
                    disabled={isSubmitting}
                  >
                    {isSubmitting ? "Submitting…" : "Request access"}
                  </Button>
                </CardFooter>
              </Form>
            </Card>
          ) : (
            <Card>
              <CardHeader>
                <CardTitle>Enable service</CardTitle>
                <CardDescription>
                  This service is available for self-service activation. Enable
                  it to start using it immediately.
                </CardDescription>
              </CardHeader>
              <CardFooter className="border-t py-3">
                <Form method="post" replace>
                  <input type="hidden" name="intent" value="enable" />
                  <Button
                    type="primary"
                    theme="solid"
                    htmlType="submit"
                    disabled={isSubmitting}
                  >
                    {isSubmitting ? "Enabling…" : "Enable service"}
                  </Button>
                </Form>
              </CardFooter>
            </Card>
          )}
        </section>
      ) : null}

      {/* Dependencies */}
      {dependencies.length > 0 ? (
        <section>
          <Alert variant="info">
            <Info className="h-4 w-4" />
            <AlertTitle>Includes dependencies</AlertTitle>
            <AlertDescription>
              Enabling this service will also enable:{" "}
              {dependencies.map((d, i) => (
                <span key={d.serviceRef.name}>
                  {i > 0 ? ", " : ""}
                  <span className="font-mono text-xs">{d.serviceRef.name}</span>
                </span>
              ))}
              .
            </AlertDescription>
          </Alert>
        </section>
      ) : null}
    </div>
  );
}
