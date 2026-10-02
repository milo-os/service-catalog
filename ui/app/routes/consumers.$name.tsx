import { json, redirect } from "@remix-run/node";
import type { ActionFunctionArgs, LoaderFunctionArgs } from "@remix-run/node";
import {
  Form,
  Link,
  useActionData,
  useLoaderData,
  useNavigation,
} from "@remix-run/react";
import { Badge } from "@datum-cloud/datum-ui/badge";
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
import { ArrowLeft, CheckCircle2, XCircle } from "lucide-react";
import { fetchK8s } from "~/lib/k8s.server";
import { consumerPhaseBadgeProps, formatDate } from "~/lib/format";
import type { Service, ServiceConsumer } from "~/lib/types";

interface LoaderData {
  consumer?: ServiceConsumer;
  serviceDisplayName?: string;
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
      { error: "Missing consumer name." } satisfies LoaderData,
      { status: 400 }
    );
  }

  try {
    const consumer = await fetchK8s<ServiceConsumer>(
      request,
      `/apis/services.miloapis.com/v1alpha1/serviceconsumers/${encodeURIComponent(name)}`
    );

    let serviceDisplayName: string | undefined;
    try {
      const svc = await fetchK8s<Service>(
        request,
        `/apis/services.miloapis.com/v1alpha1/services/${encodeURIComponent(consumer.spec.serviceRef.name)}`
      );
      serviceDisplayName = svc.spec.displayName;
    } catch {
      // fall back to raw resource name in the component
    }

    return json({ consumer, serviceDisplayName } satisfies LoaderData);
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    const is404 =
      message.includes("404") || message.toLowerCase().includes("not found");
    return json(
      { error: is404 ? "Access request not found." : message } satisfies LoaderData,
      { status: is404 ? 404 : 500 }
    );
  }
}

export async function action({ request, params }: ActionFunctionArgs) {
  const name = params.name;
  if (!name) {
    return json(
      { ok: false, error: "Missing consumer name." } satisfies ActionData,
      { status: 400 }
    );
  }

  const form = await request.formData();
  const intent = String(form.get("intent") ?? "");
  const approvalMessage = String(form.get("approvalMessage") ?? "").trim();

  if (intent !== "approve" && intent !== "deny") {
    return json(
      { ok: false, error: `Unknown intent: ${intent}` } satisfies ActionData,
      { status: 400 }
    );
  }

  const decision = intent === "approve" ? "Approved" : "Denied";

  try {
    await fetchK8s<ServiceConsumer>(
      request,
      `/apis/services.miloapis.com/v1alpha1/serviceconsumers/${encodeURIComponent(name)}`,
      {
        method: "PATCH",
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({
          spec: {
            approval: {
              decision,
              message: approvalMessage || undefined,
            },
          },
        }),
      }
    );
    return redirect("/consumers");
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

function DefRow({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <dt className="text-sm font-medium text-muted-foreground">{label}</dt>
      <dd className="text-sm text-foreground mt-1">{children}</dd>
    </div>
  );
}

export default function ConsumerDetail() {
  const { consumer, serviceDisplayName, error } = useLoaderData<typeof loader>() as LoaderData;
  const actionData = useActionData<typeof action>() as ActionData | undefined;
  const navigation = useNavigation();

  const isSubmitting = navigation.state !== "idle";
  const submittingIntent = navigation.formData?.get("intent") as
    | "approve"
    | "deny"
    | null;

  if (error || !consumer) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4">
        <Link
          to="/consumers"
          className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors w-fit"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to access requests
        </Link>
        <Card>
          <CardHeader>
            <CardTitle>Request not found</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              {error ?? "This access request could not be loaded."}
            </p>
            <Link
              to="/consumers"
              className="text-sm text-primary underline mt-2 inline-block"
            >
              Back to access requests
            </Link>
          </CardContent>
        </Card>
      </div>
    );
  }

  const phase = consumer.status?.phase;
  const badge = consumerPhaseBadgeProps(phase ?? "");
  const isPending = phase === "PendingApproval";
  const isApproved = consumer.spec.approval?.decision === "Approved";
  const isDenied = consumer.spec.approval?.decision === "Denied";

  return (
    <div className="flex flex-col gap-4 px-6 py-4 max-w-2xl">
      <Link
        to="/consumers"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors w-fit"
      >
        <ArrowLeft className="h-4 w-4" />
        Back to access requests
      </Link>

      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold text-foreground">Access Request</h1>
        <Badge type={badge.type} theme={badge.theme}>
          {badge.label}
        </Badge>
      </div>

      {/* Request Details */}
      <Card>
        <CardHeader>
          <CardTitle>Request details</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-8 gap-y-4">
            <DefRow label="Consumer project">
              <span className="font-mono text-xs">
                {consumer.spec.consumerProjectRef.name}
              </span>
            </DefRow>
            <DefRow label="Service">
              <Link
                to={`/services/${encodeURIComponent(consumer.spec.serviceRef.name)}`}
                className="text-primary hover:underline"
              >
                {serviceDisplayName ?? consumer.spec.serviceRef.name}
              </Link>
            </DefRow>
            <DefRow label="Requested">
              {formatDate(consumer.metadata.creationTimestamp)}
            </DefRow>
            {consumer.status?.entitledAt ? (
              <DefRow label="Entitled at">
                {formatDate(consumer.status.entitledAt)}
              </DefRow>
            ) : null}
          </dl>
        </CardContent>
      </Card>

      {/* Pending: decision form */}
      {isPending ? (
        <Card>
          <CardHeader>
            <CardTitle>Decision</CardTitle>
            <CardDescription>
              Approve to grant access, or deny to reject this request.
            </CardDescription>
          </CardHeader>
          <Form method="post" replace>
            <CardContent className="flex flex-col gap-3">
              {actionData && !actionData.ok && actionData.error ? (
                <Alert variant="destructive">
                  <XCircle className="h-4 w-4" />
                  <AlertTitle>Something went wrong</AlertTitle>
                  <AlertDescription>{actionData.error}</AlertDescription>
                </Alert>
              ) : null}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="approvalMessage">
                  Note to consumer{" "}
                  <span className="text-muted-foreground font-normal">
                    (optional)
                  </span>
                </Label>
                <Textarea
                  id="approvalMessage"
                  name="approvalMessage"
                  rows={3}
                  placeholder="Add a note to the consumer (optional)"
                  disabled={isSubmitting}
                />
              </div>
            </CardContent>
            <CardFooter className="gap-3 border-t py-3">
              <button
                name="intent"
                value="approve"
                type="submit"
                disabled={isSubmitting}
                className="inline-flex items-center gap-2 h-9 px-4 rounded-md text-sm font-medium bg-success-600 text-white hover:bg-success-700 disabled:opacity-50 transition-colors"
              >
                <CheckCircle2 className="h-4 w-4" />
                {isSubmitting && submittingIntent === "approve"
                  ? "Approving…"
                  : "Approve"}
              </button>
              <button
                name="intent"
                value="deny"
                type="submit"
                disabled={isSubmitting}
                className="inline-flex items-center gap-2 h-9 px-4 rounded-md text-sm font-medium border border-input bg-background text-foreground hover:bg-muted disabled:opacity-50 transition-colors"
              >
                <XCircle className="h-4 w-4 text-muted-foreground" />
                {isSubmitting && submittingIntent === "deny"
                  ? "Denying…"
                  : "Deny"}
              </button>
            </CardFooter>
          </Form>
        </Card>
      ) : null}

      {/* Already decided */}
      {(isApproved || isDenied) && !isPending ? (
        <Card>
          <CardHeader>
            <CardTitle>Decision</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              {isApproved ? (
                <CheckCircle2 className="h-5 w-5 text-success-600" />
              ) : (
                <XCircle className="h-5 w-5 text-muted-foreground" />
              )}
              <span className="text-sm font-medium text-foreground">
                {consumer.spec.approval!.decision}
              </span>
            </div>
            {consumer.spec.approval?.message ? (
              <p className="text-sm text-muted-foreground rounded-md border bg-muted/40 px-3 py-2">
                {consumer.spec.approval.message}
              </p>
            ) : null}
            {isDenied ? (
              <p className="text-xs text-muted-foreground">
                Once denied, the consumer must resubmit to request access again.
              </p>
            ) : null}
          </CardContent>
        </Card>
      ) : null}

      {/* Conditions */}
      {consumer.status?.conditions && consumer.status.conditions.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>Conditions</CardTitle>
          </CardHeader>
          <CardContent>
            <ul className="flex flex-col gap-2">
              {consumer.status.conditions.map((cond) => (
                <li
                  key={cond.type}
                  className="flex flex-col gap-0.5 text-sm"
                >
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-foreground">
                      {cond.type}
                    </span>
                    <span className="text-muted-foreground">{cond.status}</span>
                    {cond.reason ? (
                      <span className="text-xs text-muted-foreground">
                        ({cond.reason})
                      </span>
                    ) : null}
                  </div>
                  {cond.message ? (
                    <p className="text-xs text-muted-foreground">
                      {cond.message}
                    </p>
                  ) : null}
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}
