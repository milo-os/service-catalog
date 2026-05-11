import { json, redirect } from "@remix-run/node";
import type {
  ActionFunctionArgs,
  LoaderFunctionArgs,
} from "@remix-run/node";
import {
  Form,
  useActionData,
  useNavigate,
  useNavigation,
  useSearchParams,
} from "@remix-run/react";
import { useMemo, useState } from "react";
import {
  Alert,
  AlertDescription,
  AlertTitle,
} from "@datum-cloud/datum-ui/alert";
import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { Checkbox } from "@datum-cloud/datum-ui/checkbox";
import { Input } from "@datum-cloud/datum-ui/input";
import { Label } from "@datum-cloud/datum-ui/label";
import { Textarea } from "@datum-cloud/datum-ui/textarea";
import { Plus, X } from "lucide-react";
import { FieldError } from "~/components/wizard/FieldError";
import { MeterCardEditor } from "~/components/wizard/MeterCardEditor";
import { MrtCardEditor } from "~/components/wizard/MrtCardEditor";
import { StepperItem } from "~/components/wizard/StepperItem";
import {
  NAME_RE,
  newMeter,
  newMrt,
  validateMeters,
  validateMrts,
  type MeterDraft,
  type MrtDraft,
} from "~/components/wizard/wizard-validation";
import { fetchK8s } from "~/lib/k8s.server";
import type { Service } from "~/lib/types";

interface WizardForm {
  serviceName: string;
  displayName: string;
  description: string;
  ownerProject: string;
  serviceNameOverridden: boolean;
  mrts: MrtDraft[];
  meters: MeterDraft[];
}

interface ActionData {
  ok: boolean;
  error?: string;
  partial?: { serviceCreated?: boolean; serviceName?: string };
}

function slugify(input: string): string {
  return input
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 63);
}

function validateStep1(form: WizardForm): Record<string, string> {
  const errors: Record<string, string> = {};
  const dn = form.displayName.trim();
  if (dn.length < 1 || dn.length > 120) {
    errors.displayName = "Display name must be 1–120 characters.";
  }
  if (form.description.length > 1000) {
    errors.description = "Description must be 1000 characters or fewer.";
  }
  if (!NAME_RE.test(form.serviceName) || form.serviceName.length > 63) {
    errors.serviceName =
      "Service name must be lowercase alphanumeric (with hyphens), start with a letter, and be ≤ 63 chars.";
  }
  if (!NAME_RE.test(form.ownerProject) || form.ownerProject.length > 63) {
    errors.ownerProject =
      "Owner project must be a lowercase DNS-1123 name (a-z, 0-9, -).";
  }
  return errors;
}

export async function loader(_args: LoaderFunctionArgs) {
  // No server data needed for the wizard form itself.
  return json({});
}

export async function action({ request }: ActionFunctionArgs) {
  const form = await request.formData();
  const raw = form.get("payload");
  if (typeof raw !== "string") {
    return json(
      { ok: false, error: "Missing wizard payload." } satisfies ActionData,
      { status: 400 }
    );
  }
  let payload: WizardForm;
  try {
    payload = JSON.parse(raw) as WizardForm;
  } catch {
    return json(
      { ok: false, error: "Invalid wizard payload." } satisfies ActionData,
      { status: 400 }
    );
  }

  // Re-validate server-side.
  const allErrors = {
    ...validateStep1(payload),
    ...validateMrts(payload.mrts),
    ...validateMeters(payload.meters, payload.mrts),
  };
  if (Object.keys(allErrors).length > 0) {
    const message = Object.values(allErrors).join(" ");
    return json(
      { ok: false, error: message } satisfies ActionData,
      { status: 400 }
    );
  }

  // 1) Create the Service
  try {
    await fetchK8s<Service>(
      request,
      "/apis/services.miloapis.com/v1alpha1/services",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          apiVersion: "services.miloapis.com/v1alpha1",
          kind: "Service",
          metadata: { name: payload.serviceName },
          spec: {
            serviceName: payload.serviceName,
            displayName: payload.displayName.trim(),
            description: payload.description ?? "",
            phase: "Draft",
            owner: {
              producerProjectRef: { name: payload.ownerProject },
            },
          },
        }),
      }
    );
  } catch (e) {
    return json(
      {
        ok: false,
        error: `Failed to create service: ${
          e instanceof Error ? e.message : String(e)
        }`,
      } satisfies ActionData,
      { status: 500 }
    );
  }

  // 2) Create the initial ServiceConfiguration. If this fails, do NOT
  //    roll back the Service — Service-without-Config is a valid state.
  const configName = `${payload.serviceName}-v1-0-0`;
  try {
    await fetchK8s(
      request,
      "/apis/services.miloapis.com/v1alpha1/serviceconfigurations",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          apiVersion: "services.miloapis.com/v1alpha1",
          kind: "ServiceConfiguration",
          metadata: { name: configName },
          spec: {
            serviceRef: { name: payload.serviceName },
            version: "1.0.0",
            phase: "Draft",
            monitoredResourceTypes: payload.mrts,
            meters: payload.meters,
          },
        }),
      }
    );
  } catch (e) {
    return json(
      {
        ok: false,
        error: `Service "${payload.serviceName}" was created but the initial configuration failed: ${
          e instanceof Error ? e.message : String(e)
        }. You can retry creating the configuration from the service detail page.`,
        partial: {
          serviceCreated: true,
          serviceName: payload.serviceName,
        },
      } satisfies ActionData,
      { status: 500 }
    );
  }

  return redirect(
    `/services/${encodeURIComponent(payload.serviceName)}?tab=configurations`
  );
}

function Step1Identity({
  form,
  setForm,
  errors,
}: {
  form: WizardForm;
  setForm: (next: WizardForm) => void;
  errors: Record<string, string>;
}) {
  const [showName, setShowName] = useState(form.serviceNameOverridden);

  const onDisplayName = (value: string) => {
    const next: WizardForm = { ...form, displayName: value };
    if (!form.serviceNameOverridden) {
      next.serviceName = slugify(value);
    }
    setForm(next);
  };
  const onServiceName = (value: string) => {
    setForm({
      ...form,
      serviceName: value,
      serviceNameOverridden: true,
    });
  };

  return (
    <div className="flex flex-col gap-4 max-w-2xl">
      <div className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Service identity</h2>
        <p className="text-sm text-muted-foreground">
          Public metadata for the new service. Service name is the immutable
          canonical key — it cannot be changed later.
        </p>
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="displayName">Display name</Label>
        <Input
          id="displayName"
          value={form.displayName}
          onChange={(e) => onDisplayName(e.target.value)}
          maxLength={120}
          required
        />
        <p className="text-xs text-muted-foreground">
          Shown in the catalog and in dashboards.
        </p>
        <FieldError message={errors.displayName} />
      </div>

      <div className="flex flex-col gap-1.5">
        <div className="flex items-baseline justify-between">
          <Label htmlFor="serviceName">Service name</Label>
          {!showName ? (
            <button
              type="button"
              className="text-xs text-primary hover:underline"
              onClick={() => setShowName(true)}
            >
              Customize
            </button>
          ) : null}
        </div>
        {showName ? (
          <Input
            id="serviceName"
            value={form.serviceName}
            onChange={(e) => onServiceName(e.target.value)}
            maxLength={63}
            required
          />
        ) : (
          <p className="text-sm">
            Service name will be:{" "}
            <strong className="font-mono">
              {form.serviceName || "—"}
            </strong>
          </p>
        )}
        <p className="text-xs text-muted-foreground">
          Lowercase letters, digits and hyphens. Used in URLs and across
          configurations.
        </p>
        <FieldError message={errors.serviceName} />
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="description">Description</Label>
        <Textarea
          id="description"
          value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })}
          rows={4}
          maxLength={1000}
        />
        <p className="text-xs text-muted-foreground">
          {form.description.length} / 1000
        </p>
        <FieldError message={errors.description} />
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="ownerProject">Owner project</Label>
        <Input
          id="ownerProject"
          value={form.ownerProject}
          onChange={(e) => setForm({ ...form, ownerProject: e.target.value })}
          maxLength={63}
          required
        />
        <p className="text-xs text-muted-foreground">
          Project that owns and operates this service.
        </p>
        <FieldError message={errors.ownerProject} />
      </div>
    </div>
  );
}

function Step2Mrts({
  form,
  setForm,
  errors,
}: {
  form: WizardForm;
  setForm: (next: WizardForm) => void;
  errors: Record<string, string>;
}) {
  const updateMrt = (index: number, next: MrtDraft) => {
    const list = [...form.mrts];
    list[index] = next;
    setForm({ ...form, mrts: list });
  };
  const removeMrt = (index: number) => {
    const list = form.mrts.filter((_, i) => i !== index);
    setForm({ ...form, mrts: list });
  };
  const addMrt = () => setForm({ ...form, mrts: [...form.mrts, newMrt()] });

  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <div className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Monitored resource types</h2>
        <p className="text-sm text-muted-foreground">
          Define the resources whose usage this service will meter. You can
          add more later by editing the configuration.
        </p>
      </div>

      {form.mrts.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="flex flex-col items-center gap-3 py-6">
            <p className="text-sm text-muted-foreground">
              No monitored resources yet.
            </p>
            <Button
              type="primary"
              theme="solid"
              htmlType="button"
              onClick={addMrt}
              icon={<Plus className="h-4 w-4" />}
            >
              Add resource type
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="flex flex-col gap-4">
          {form.mrts.map((mrt, i) => (
            <MrtCardEditor
              key={i}
              index={i}
              mrt={mrt}
              errors={errors}
              onChange={(next) => updateMrt(i, next)}
              onRemove={() => removeMrt(i)}
            />
          ))}
          <div>
            <Button
              type="secondary"
              theme="outline"
              htmlType="button"
              onClick={addMrt}
              icon={<Plus className="h-4 w-4" />}
            >
              Add another
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function Step3Meters({
  form,
  setForm,
  errors,
}: {
  form: WizardForm;
  setForm: (next: WizardForm) => void;
  errors: Record<string, string>;
}) {
  const mrtOptions = useMemo(
    () =>
      form.mrts
        .filter((m) => m.type)
        .map((m) => ({
          value: m.type,
          label: m.displayName ? `${m.type} · ${m.displayName}` : m.type,
        })),
    [form.mrts]
  );
  const updateMeter = (index: number, next: MeterDraft) => {
    const list = [...form.meters];
    list[index] = next;
    setForm({ ...form, meters: list });
  };
  const removeMeter = (index: number) => {
    const list = form.meters.filter((_, i) => i !== index);
    setForm({ ...form, meters: list });
  };
  const addMeter = () =>
    setForm({ ...form, meters: [...form.meters, newMeter()] });

  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <div className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Meters</h2>
        <p className="text-sm text-muted-foreground">
          Declare what this service measures. Each meter aggregates over one
          or more monitored resources from step 2.
        </p>
      </div>

      {form.meters.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="flex flex-col items-center gap-3 py-6">
            <p className="text-sm text-muted-foreground">No meters yet.</p>
            <Button
              type="primary"
              theme="solid"
              htmlType="button"
              onClick={addMeter}
              icon={<Plus className="h-4 w-4" />}
            >
              Add meter
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="flex flex-col gap-4">
          {form.meters.map((meter, i) => (
            <MeterCardEditor
              key={i}
              index={i}
              meter={meter}
              mrtOptions={mrtOptions}
              errors={errors}
              onChange={(next) => updateMeter(i, next)}
              onRemove={() => removeMeter(i)}
            />
          ))}
          <div>
            <Button
              type="secondary"
              theme="outline"
              htmlType="button"
              onClick={addMeter}
              icon={<Plus className="h-4 w-4" />}
            >
              Add another
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function Step4Review({ form }: { form: WizardForm }) {
  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <div className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Review &amp; create</h2>
        <p className="text-sm text-muted-foreground">
          Confirm the values below. The service and its first configuration
          are created in Draft.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Service</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-8 gap-y-3">
            <div>
              <dt className="text-xs uppercase tracking-wider text-muted-foreground">
                Service name
              </dt>
              <dd className="font-mono text-sm">
                {form.serviceName || "—"}
              </dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wider text-muted-foreground">
                Display name
              </dt>
              <dd className="text-sm">{form.displayName || "—"}</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wider text-muted-foreground">
                Owner project
              </dt>
              <dd className="font-mono text-sm">
                {form.ownerProject || "—"}
              </dd>
            </div>
            <div className="col-span-2">
              <dt className="text-xs uppercase tracking-wider text-muted-foreground">
                Description
              </dt>
              <dd className="text-sm whitespace-pre-line">
                {form.description || "—"}
              </dd>
            </div>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Initial configuration · v1.0.0 (Draft)</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-5">
          <div>
            <p className="text-xs uppercase tracking-wider text-muted-foreground mb-1">
              Monitored resource types ({form.mrts.length})
            </p>
            {form.mrts.length === 0 ? (
              <p className="text-sm text-muted-foreground">None.</p>
            ) : (
              <ul className="list-disc pl-5 text-sm">
                {form.mrts.map((mrt) => (
                  <li key={mrt.type}>
                    <span className="font-mono text-xs">{mrt.type}</span>
                    {mrt.displayName ? ` — ${mrt.displayName}` : null}
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div>
            <p className="text-xs uppercase tracking-wider text-muted-foreground mb-1">
              Meters ({form.meters.length})
            </p>
            {form.meters.length === 0 ? (
              <p className="text-sm text-muted-foreground">None.</p>
            ) : (
              <ul className="list-disc pl-5 text-sm">
                {form.meters.map((m) => (
                  <li key={m.name}>
                    <span className="font-mono text-xs">{m.name}</span>
                    {m.displayName ? ` — ${m.displayName}` : null}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </CardContent>
      </Card>

      <Alert variant="info">
        <AlertTitle>Created in Draft</AlertTitle>
        <AlertDescription>
          The service and its first configuration will be created in{" "}
          <strong>Draft</strong>. Publish from the Configurations tab when
          you're ready to expose it to consumers.
        </AlertDescription>
      </Alert>
    </div>
  );
}

const STEP_LABELS = [
  "Service identity",
  "Monitored resource types",
  "Meters",
  "Review & create",
];

export default function NewService() {
  const [searchParams, setSearchParams] = useSearchParams();
  const navigation = useNavigation();
  const navigate = useNavigate();
  const actionData = useActionData<typeof action>() as ActionData | undefined;

  const stepParam = parseInt(searchParams.get("step") ?? "1", 10);
  const step =
    Number.isFinite(stepParam) && stepParam >= 1 && stepParam <= 4
      ? stepParam
      : 1;
  const goToStep = (next: number) => {
    const sp = new URLSearchParams(searchParams);
    sp.set("step", String(next));
    setSearchParams(sp, { replace: false });
  };

  const [form, setForm] = useState<WizardForm>({
    serviceName: "",
    displayName: "",
    description: "",
    ownerProject: "",
    serviceNameOverridden: false,
    mrts: [],
    meters: [],
  });
  const [consented, setConsented] = useState(false);

  const isDirty =
    form.displayName.length > 0 ||
    form.description.length > 0 ||
    form.ownerProject.length > 0 ||
    form.serviceName.length > 0 ||
    form.mrts.length > 0 ||
    form.meters.length > 0;

  const submitting = navigation.state === "submitting";

  const errors = useMemo(() => {
    if (step === 1) return validateStep1(form);
    if (step === 2) return validateMrts(form.mrts);
    if (step === 3) return validateMeters(form.meters, form.mrts);
    return {};
  }, [step, form]);

  const next = () => {
    if (Object.keys(errors).length > 0) return;
    goToStep(step + 1);
  };
  const back = () => {
    if (step > 1) goToStep(step - 1);
  };

  const confirmClose = () => {
    if (
      !isDirty ||
      window.confirm("Discard changes? Wizard state will be lost.")
    ) {
      navigate("/services");
    }
  };

  return (
    <div className="grid h-[calc(100vh-3rem)] grid-rows-[auto_1fr]">
      {/* Header */}
      <header className="flex items-center justify-between border-b border-border/50 px-6 py-3">
        <div>
          <h1 className="text-xl font-bold">New service</h1>
          <p className="text-sm text-muted-foreground">
            Create a service definition and its first configuration.
          </p>
        </div>
        <Button
          type="secondary"
          theme="borderless"
          size="icon"
          htmlType="button"
          aria-label="Close"
          onClick={confirmClose}
          icon={<X className="h-5 w-5" />}
        />
      </header>

      {/* Body */}
      <div className="grid grid-cols-[220px_1fr] overflow-hidden">
        <aside className="border-r border-border/50 bg-muted/20 p-4">
          <p className="mb-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Steps
          </p>
          <ol className="flex flex-col gap-1">
            {STEP_LABELS.map((label, idx) => (
              <StepperItem
                key={label}
                n={idx + 1}
                label={label}
                active={step === idx + 1}
                done={step > idx + 1}
              />
            ))}
          </ol>
        </aside>

        <div className="flex flex-col">
          <main className="flex-1 overflow-auto px-6 py-6">
            {actionData && !actionData.ok ? (
              <Alert variant="destructive" className="mb-4">
                <AlertTitle>Couldn't create service</AlertTitle>
                <AlertDescription>{actionData.error}</AlertDescription>
              </Alert>
            ) : null}

            {step === 1 ? (
              <Step1Identity
                form={form}
                setForm={setForm}
                errors={errors}
              />
            ) : null}
            {step === 2 ? (
              <Step2Mrts form={form} setForm={setForm} errors={errors} />
            ) : null}
            {step === 3 ? (
              <Step3Meters form={form} setForm={setForm} errors={errors} />
            ) : null}
            {step === 4 ? (
              <div className="flex flex-col gap-4">
                <Step4Review form={form} />
                <label className="flex items-start gap-3 cursor-pointer max-w-3xl">
                  <Checkbox
                    checked={consented}
                    onCheckedChange={(checked) =>
                      setConsented(checked === true)
                    }
                  />
                  <span className="text-sm text-foreground">
                    I confirm the service name and owner project are
                    correct. The service name cannot be changed after
                    creation.
                  </span>
                </label>
              </div>
            ) : null}
          </main>

          {/* Footer */}
          <footer className="flex items-center justify-between border-t border-border/50 px-6 py-3 bg-card/50">
            <Button
              type="secondary"
              theme="borderless"
              htmlType="button"
              disabled={step === 1 || submitting}
              onClick={back}
            >
              ← Back
            </Button>
            <div className="flex items-center gap-3">
              {step < 4 ? (
                <Button
                  type="primary"
                  theme="solid"
                  htmlType="button"
                  disabled={Object.keys(errors).length > 0}
                  onClick={next}
                >
                  Next →
                </Button>
              ) : (
                <Form method="post">
                  <input
                    type="hidden"
                    name="payload"
                    value={JSON.stringify(form)}
                  />
                  <Button
                    type="primary"
                    theme="solid"
                    htmlType="submit"
                    disabled={!consented || submitting}
                  >
                    {submitting ? "Creating…" : "Create service"}
                  </Button>
                </Form>
              )}
            </div>
          </footer>
        </div>
      </div>
    </div>
  );
}
