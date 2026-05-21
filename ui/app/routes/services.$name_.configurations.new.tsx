import { json, redirect } from "@remix-run/node";
import type {
  ActionFunctionArgs,
  LoaderFunctionArgs,
} from "@remix-run/node";
import {
  Form,
  useActionData,
  useLoaderData,
  useNavigate,
  useNavigation,
  useParams,
  useSearchParams,
} from "@remix-run/react";
import { useEffect, useMemo, useState } from "react";
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
import {
  RadioGroup,
  RadioGroupItem,
} from "@datum-cloud/datum-ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@datum-cloud/datum-ui/select";
import { Lightbulb, Plus, X } from "lucide-react";
import { FieldError } from "~/components/wizard/FieldError";
import { MeterCardEditor } from "~/components/wizard/MeterCardEditor";
import { MrtCardEditor } from "~/components/wizard/MrtCardEditor";
import { StepperItem } from "~/components/wizard/StepperItem";
import {
  newMeter,
  newMrt,
  suggestNextVersion,
  validateConfigVersion,
  validateMeters,
  validateMrts,
  type MeterDraft,
  type MrtDraft,
} from "~/components/wizard/wizard-validation";
import {
  countDiff,
  diffByKey,
  totalChanged,
  type DiffCounts,
} from "~/lib/diff";
import { phaseBadgeProps } from "~/lib/format";
import { fetchK8s } from "~/lib/k8s.server";
import type {
  KubeList,
  Service,
  ServiceConfiguration,
} from "~/lib/types";

interface LoaderData {
  service?: Service;
  configs: ServiceConfiguration[];
  existingVersions: string[];
  previousVersion: string | null;
  suggestedVersion: string;
  error?: string;
}

interface ActionData {
  ok: boolean;
  error?: string;
}

interface ConfigWizardForm {
  version: string;
  source: "blank" | "clone";
  cloneFrom: string;
  mrts: MrtDraft[];
  meters: MeterDraft[];
}

const STEP_LABELS = [
  "Version & source",
  "Monitored resource types",
  "Meters",
  "Review & create",
];

function configMetadataName(serviceName: string, version: string): string {
  return `${serviceName}-v${version.replace(/\./g, "-")}`;
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const name = params.name;
  if (!name) {
    return json(
      {
        error: "Missing service name.",
        configs: [],
        existingVersions: [],
        previousVersion: null,
        suggestedVersion: "1.0.0",
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
    mine.sort((a, b) => {
      const at =
        a.status?.publishedAt ?? a.metadata.creationTimestamp ?? "";
      const bt =
        b.status?.publishedAt ?? b.metadata.creationTimestamp ?? "";
      return new Date(bt).getTime() - new Date(at).getTime();
    });
    const existingVersions = mine
      .map((c) => c.spec.version)
      .filter((v): v is string => !!v);
    const previousVersion = existingVersions[0] ?? null;
    const suggestedVersion = suggestNextVersion(previousVersion);
    return json({
      service,
      configs: mine,
      existingVersions,
      previousVersion,
      suggestedVersion,
    } satisfies LoaderData);
  } catch (e) {
    console.error('[configurations/new loader]', e);
    return json({
      error: e instanceof Error ? e.message : String(e),
      configs: [],
      existingVersions: [],
      previousVersion: null,
      suggestedVersion: "1.0.0",
    } satisfies LoaderData);
  }
}

export async function action({ request, params }: ActionFunctionArgs) {
  const formData = await request.formData();
  const raw = formData.get("payload");
  const serviceName = params.name;
  if (!serviceName) {
    return json(
      { ok: false, error: "Missing service name." } satisfies ActionData,
      { status: 400 }
    );
  }
  if (typeof raw !== "string") {
    return json(
      {
        ok: false,
        error: "Missing wizard payload.",
      } satisfies ActionData,
      { status: 400 }
    );
  }

  let payload: ConfigWizardForm;
  try {
    payload = JSON.parse(raw) as ConfigWizardForm;
  } catch {
    return json(
      {
        ok: false,
        error: "Invalid wizard payload.",
      } satisfies ActionData,
      { status: 400 }
    );
  }

  // Re-fetch existing configs to re-validate version uniqueness server-side.
  let existingVersions: string[] = [];
  let cloneOptions: string[] = [];
  try {
    const configList = await fetchK8s<KubeList<ServiceConfiguration>>(
      request,
      `/apis/services.miloapis.com/v1alpha1/serviceconfigurations`
    );
    const mine = (configList.items ?? []).filter(
      (c) => c.spec?.serviceRef?.name === serviceName
    );
    existingVersions = mine
      .map((c) => c.spec.version)
      .filter((v): v is string => !!v);
    cloneOptions = mine.map((c) => c.metadata.name);
  } catch (e) {
    return json(
      {
        ok: false,
        error: `Could not verify existing configurations: ${
          e instanceof Error ? e.message : String(e)
        }`,
      } satisfies ActionData,
      { status: 500 }
    );
  }

  const allErrors = {
    ...validateConfigVersion(payload, existingVersions, cloneOptions),
    ...validateMrts(payload.mrts),
    ...validateMeters(payload.meters, payload.mrts),
  };
  if (Object.keys(allErrors).length > 0) {
    return json(
      {
        ok: false,
        error: Object.values(allErrors).join(" "),
      } satisfies ActionData,
      { status: 400 }
    );
  }

  const configName = configMetadataName(serviceName, payload.version);
  if (configName.length > 253) {
    return json(
      {
        ok: false,
        error: `Resulting resource name "${configName}" exceeds 253 characters.`,
      } satisfies ActionData,
      { status: 400 }
    );
  }

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
            serviceRef: { name: serviceName },
            version: payload.version,
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
        error: `Failed to create configuration: ${
          e instanceof Error ? e.message : String(e)
        }`,
      } satisfies ActionData,
      { status: 500 }
    );
  }

  return redirect(
    `/services/${encodeURIComponent(serviceName)}?tab=configurations`
  );
}

function deepClone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function mrtFieldsForDiff(m: {
  type: string;
  displayName?: string;
  description?: string;
  gvk: { group: string; kind: string };
  labels?: { name: string; description?: string }[] | string[];
}): Record<string, unknown> {
  const labels = Array.isArray(m.labels)
    ? m.labels.map((l) => (typeof l === "string" ? l : l.name)).sort()
    : [];
  return {
    type: m.type ?? "",
    displayName: m.displayName ?? "",
    description: m.description ?? "",
    group: m.gvk?.group ?? "",
    kind: m.gvk?.kind ?? "",
    labels,
  };
}

function meterFieldsForDiff(m: {
  name: string;
  displayName?: string;
  description?: string;
  measurement: { aggregation: string; unit: string };
  billing: { consumedUnit: string; pricingUnit: string };
  monitoredResourceTypes: string[];
}): Record<string, unknown> {
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

function summariseCounts(counts: DiffCounts): string {
  if (totalChanged(counts) === 0) {
    return counts.unchanged > 0
      ? `no changes · ${counts.unchanged} unchanged`
      : "no changes";
  }
  const parts: string[] = [];
  if (counts.added) parts.push(`+${counts.added} added`);
  if (counts.removed) parts.push(`−${counts.removed} removed`);
  if (counts.modified) parts.push(`${counts.modified} modified`);
  if (counts.unchanged) parts.push(`${counts.unchanged} unchanged`);
  return parts.join(" · ");
}

function ChangesSummary({
  cloneSource,
  formMrts,
  formMeters,
}: {
  cloneSource: ServiceConfiguration;
  formMrts: MrtDraft[];
  formMeters: MeterDraft[];
}) {
  const mrtDiff = useMemo(
    () =>
      diffByKey(
        cloneSource.spec.monitoredResourceTypes,
        formMrts,
        (m) => m.type,
        mrtFieldsForDiff
      ),
    [cloneSource, formMrts]
  );
  const meterDiff = useMemo(
    () =>
      diffByKey(
        cloneSource.spec.meters,
        formMeters,
        (m) => m.name,
        meterFieldsForDiff
      ),
    [cloneSource, formMeters]
  );
  const mrtCounts = countDiff(mrtDiff);
  const meterCounts = countDiff(meterDiff);
  const totallyIdentical =
    totalChanged(mrtCounts) === 0 && totalChanged(meterCounts) === 0;
  if (totallyIdentical) {
    return (
      <p className="text-sm text-muted-foreground">
        Identical to v{cloneSource.spec.version}.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-1">
      <p className="text-sm">
        <span className="font-medium">Monitored resource types:</span>{" "}
        <span className="text-muted-foreground">
          {summariseCounts(mrtCounts)}
        </span>
      </p>
      <p className="text-sm">
        <span className="font-medium">Meters:</span>{" "}
        <span className="text-muted-foreground">
          {summariseCounts(meterCounts)}
        </span>
      </p>
    </div>
  );
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
      <dt className="text-xs uppercase tracking-wider text-muted-foreground">
        {label}
      </dt>
      <dd className="text-sm text-foreground mt-1 whitespace-pre-line">
        {children}
      </dd>
    </div>
  );
}

function Step1VersionSource({
  form,
  setForm,
  errors,
  configs,
  previousVersion,
  suggestedVersion,
  serviceName,
}: {
  form: ConfigWizardForm;
  setForm: (next: ConfigWizardForm) => void;
  errors: Record<string, string>;
  configs: ServiceConfiguration[];
  previousVersion: string | null;
  suggestedVersion: string;
  serviceName: string;
}) {
  const onSourceChange = (value: string) => {
    if (value !== "blank" && value !== "clone") return;
    if (value === "blank") {
      setForm({ ...form, source: "blank", cloneFrom: "", mrts: [], meters: [] });
      return;
    }
    setForm({ ...form, source: "clone" });
  };
  const onCloneChange = (cloneFrom: string) => {
    const source = configs.find((c) => c.metadata.name === cloneFrom);
    if (!source) {
      setForm({ ...form, cloneFrom });
      return;
    }
    const mrts: MrtDraft[] = (source.spec.monitoredResourceTypes ?? []).map(
      (m) =>
        deepClone({
          type: m.type,
          displayName: m.displayName ?? "",
          description: m.description ?? "",
          gvk: { group: m.gvk?.group ?? "", kind: m.gvk?.kind ?? "" },
          labels: (m.labels ?? []).map((l) => l.name),
        })
    );
    const meters: MeterDraft[] = (source.spec.meters ?? []).map((m) =>
      deepClone({
        name: m.name,
        displayName: m.displayName ?? "",
        description: m.description ?? "",
        measurement: {
          aggregation: m.measurement?.aggregation ?? "Sum",
          unit: m.measurement?.unit ?? "",
          unitDisplayName: m.measurement?.unitDisplayName ?? "",
        },
        billing: {
          consumedUnit: m.billing?.consumedUnit ?? "",
          consumedUnitDisplayName: m.billing?.consumedUnitDisplayName ?? "",
          pricingUnit: m.billing?.pricingUnit ?? "",
          pricingUnitDisplayName: m.billing?.pricingUnitDisplayName ?? "",
        },
        monitoredResourceTypes: m.monitoredResourceTypes ?? [],
      })
    );
    setForm({ ...form, cloneFrom, mrts, meters });
  };

  const previewName = form.version
    ? configMetadataName(serviceName, form.version)
    : "—";

  return (
    <div className="flex flex-col gap-4 max-w-2xl">
      <header className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Version &amp; source</h2>
        <p className="text-sm text-muted-foreground">
          Pick a version for this configuration. Optionally clone the
          monitored resources and meters from an existing version to
          get started.
        </p>
      </header>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="version">Version</Label>
        <Input
          id="version"
          value={form.version}
          onChange={(e) => setForm({ ...form, version: e.target.value })}
          placeholder="e.g. 1.2.0"
          maxLength={32}
        />
        <p className="text-xs text-muted-foreground">
          Use semantic versioning.{" "}
          {previousVersion
            ? `Previous version: ${previousVersion}.`
            : "This will be the first version."}{" "}
          Resource name will be{" "}
          <span className="font-mono">{previewName}</span>.
        </p>
        <FieldError message={errors.version} />
      </div>

      {suggestedVersion && form.version !== suggestedVersion ? (
        <Alert variant="info">
          <Lightbulb className="h-4 w-4" />
          <AlertTitle>Version suggestion</AlertTitle>
          <AlertDescription className="flex items-center gap-3">
            <span>
              Based on the latest version, we suggest{" "}
              <strong>{suggestedVersion}</strong>.
            </span>
            <button
              type="button"
              className="text-sm underline"
              onClick={() =>
                setForm({ ...form, version: suggestedVersion })
              }
            >
              Use this
            </button>
          </AlertDescription>
        </Alert>
      ) : null}

      <div className="flex flex-col gap-3">
        <Label>Start from</Label>
        <RadioGroup
          value={form.source}
          onValueChange={(value) => onSourceChange(value)}
        >
          <label className="flex items-start gap-3 cursor-pointer">
            <RadioGroupItem value="blank" />
            <div>
              <p className="text-sm font-medium">Blank</p>
              <p className="text-xs text-muted-foreground">
                Start with no monitored resources or meters.
              </p>
            </div>
          </label>
          <label
            className={`flex items-start gap-3 ${configs.length === 0 ? "cursor-not-allowed opacity-60" : "cursor-pointer"}`}
          >
            <RadioGroupItem
              value="clone"
              disabled={configs.length === 0}
            />
            <div>
              <p className="text-sm font-medium">
                Clone an existing version
              </p>
              <p className="text-xs text-muted-foreground">
                {configs.length === 0
                  ? "No prior configurations to clone from."
                  : "Copy MRTs and meters from a previous configuration; you can edit them in the next steps."}
              </p>
            </div>
          </label>
        </RadioGroup>
        <FieldError message={errors.source} />

        {form.source === "clone" ? (
          <div className="flex flex-col gap-1.5 max-w-md">
            <Label htmlFor="cloneFrom">Clone source</Label>
            <Select
              value={form.cloneFrom || ""}
              onValueChange={onCloneChange}
            >
              <SelectTrigger id="cloneFrom">
                <SelectValue placeholder="Select a version…" />
              </SelectTrigger>
              <SelectContent>
                {configs.map((c) => {
                  const phase = phaseBadgeProps(c.spec.phase);
                  const versionLabel = c.spec.version
                    ? `v${c.spec.version}`
                    : c.metadata.name;
                  return (
                    <SelectItem
                      key={c.metadata.name}
                      value={c.metadata.name}
                    >
                      {versionLabel} ({phase.label})
                    </SelectItem>
                  );
                })}
              </SelectContent>
            </Select>
            <FieldError message={errors.cloneFrom} />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function Step2Mrts({
  mrts,
  onChange,
  errors,
}: {
  mrts: MrtDraft[];
  onChange: (next: MrtDraft[]) => void;
  errors: Record<string, string>;
}) {
  const update = (index: number, next: MrtDraft) => {
    const list = [...mrts];
    list[index] = next;
    onChange(list);
  };
  const remove = (index: number) =>
    onChange(mrts.filter((_, i) => i !== index));
  const add = () => onChange([...mrts, newMrt()]);

  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <header className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Monitored resource types</h2>
        <p className="text-sm text-muted-foreground">
          Define the resources whose usage this configuration will meter.
        </p>
      </header>

      {mrts.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="flex flex-col items-center gap-3 py-6">
            <p className="text-sm text-muted-foreground">
              No monitored resources yet.
            </p>
            <Button
              type="primary"
              theme="solid"
              htmlType="button"
              onClick={add}
              icon={<Plus className="h-4 w-4" />}
            >
              Add resource type
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="flex flex-col gap-4">
          {mrts.map((mrt, i) => (
            <MrtCardEditor
              key={i}
              index={i}
              mrt={mrt}
              errors={errors}
              onChange={(next) => update(i, next)}
              onRemove={() => remove(i)}
            />
          ))}
          <div>
            <Button
              type="secondary"
              theme="outline"
              htmlType="button"
              onClick={add}
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
  meters,
  mrts,
  onChange,
  errors,
}: {
  meters: MeterDraft[];
  mrts: MrtDraft[];
  onChange: (next: MeterDraft[]) => void;
  errors: Record<string, string>;
}) {
  const mrtOptions = useMemo(
    () =>
      mrts
        .filter((m) => m.type)
        .map((m) => ({
          value: m.type,
          label: m.displayName ? `${m.type} · ${m.displayName}` : m.type,
        })),
    [mrts]
  );
  const update = (index: number, next: MeterDraft) => {
    const list = [...meters];
    list[index] = next;
    onChange(list);
  };
  const remove = (index: number) =>
    onChange(meters.filter((_, i) => i !== index));
  const add = () => onChange([...meters, newMeter()]);

  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <header className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Meters</h2>
        <p className="text-sm text-muted-foreground">
          Declare what this configuration measures. Each meter aggregates
          over one or more monitored resources from the previous step.
        </p>
      </header>

      {meters.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="flex flex-col items-center gap-3 py-6">
            <p className="text-sm text-muted-foreground">No meters yet.</p>
            <Button
              type="primary"
              theme="solid"
              htmlType="button"
              onClick={add}
              icon={<Plus className="h-4 w-4" />}
            >
              Add meter
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="flex flex-col gap-4">
          {meters.map((meter, i) => (
            <MeterCardEditor
              key={i}
              index={i}
              meter={meter}
              mrtOptions={mrtOptions}
              errors={errors}
              onChange={(next) => update(i, next)}
              onRemove={() => remove(i)}
            />
          ))}
          <div>
            <Button
              type="secondary"
              theme="outline"
              htmlType="button"
              onClick={add}
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

function Step4Review({
  form,
  service,
  cloneSource,
}: {
  form: ConfigWizardForm;
  service: Service;
  cloneSource: ServiceConfiguration | undefined;
}) {
  const cloneSourceVersion = cloneSource?.spec.version ?? "";
  return (
    <div className="flex flex-col gap-4 max-w-3xl">
      <header className="flex flex-col gap-1">
        <h2 className="text-xl font-semibold">Review &amp; create</h2>
        <p className="text-sm text-muted-foreground">
          Confirm the values below. The new configuration is created in{" "}
          <strong>Draft</strong>.
        </p>
      </header>

      <Card>
        <CardHeader>
          <CardTitle>
            Configuration v{form.version || "?"} (Draft)
          </CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-8 gap-y-3">
            <DefRow label="Service">
              {service.spec.displayName || service.metadata.name} (
              <code className="font-mono text-xs">
                {service.spec.serviceName}
              </code>
              )
            </DefRow>
            <DefRow label="Version">{form.version || "—"}</DefRow>
            <DefRow label="Source">
              {form.source === "clone" && cloneSourceVersion
                ? `Cloned from v${cloneSourceVersion}`
                : "Blank"}
            </DefRow>
            <DefRow label="Phase">Draft</DefRow>
            <DefRow label="Monitored resource types">
              {form.mrts.length}
            </DefRow>
            <DefRow label="Meters">{form.meters.length}</DefRow>
            <DefRow label="Resource name" span={2}>
              <span className="font-mono text-xs">
                {form.version
                  ? configMetadataName(
                      service.spec.serviceName,
                      form.version
                    )
                  : "—"}
              </span>
            </DefRow>
          </dl>
        </CardContent>
      </Card>

      {form.source === "clone" && cloneSource ? (
        <Card>
          <CardHeader>
            <CardTitle>Changes from v{cloneSourceVersion}</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <ChangesSummary
              cloneSource={cloneSource}
              formMrts={form.mrts}
              formMeters={form.meters}
            />
          </CardContent>
        </Card>
      ) : null}

      <Alert variant="info">
        <AlertTitle>Created in Draft</AlertTitle>
        <AlertDescription>
          The new configuration is created in <strong>Draft</strong>.
          Publish from the Configurations tab when you&rsquo;re ready.
        </AlertDescription>
      </Alert>
    </div>
  );
}

export default function NewConfiguration() {
  const data = useLoaderData<typeof loader>() as LoaderData;
  const params = useParams();
  const navigate = useNavigate();
  const navigation = useNavigation();
  const actionData = useActionData<typeof action>() as ActionData | undefined;
  const [searchParams, setSearchParams] = useSearchParams();

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

  const serviceParam = params.name ?? "";
  const cancelHref = `/services/${encodeURIComponent(serviceParam)}?tab=configurations`;

  const [form, setForm] = useState<ConfigWizardForm>({
    version: data.suggestedVersion,
    source: "blank",
    cloneFrom: "",
    mrts: [],
    meters: [],
  });
  const [consented, setConsented] = useState(false);

  // Re-seed the suggested version when the loader data first arrives /
  // changes (e.g. after a 404 retry).
  useEffect(() => {
    setForm((prev) =>
      prev.version === "" ? { ...prev, version: data.suggestedVersion } : prev
    );
  }, [data.suggestedVersion]);

  const submitting = navigation.state === "submitting";

  const errors = useMemo(() => {
    if (step === 1) {
      return validateConfigVersion(
        form,
        data.existingVersions,
        data.configs.map((c) => c.metadata.name)
      );
    }
    if (step === 2) return validateMrts(form.mrts);
    if (step === 3) return validateMeters(form.meters, form.mrts);
    return {};
  }, [step, form, data.existingVersions, data.configs]);

  const isDirty =
    form.version !== data.suggestedVersion ||
    form.source !== "blank" ||
    form.mrts.length > 0 ||
    form.meters.length > 0;

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
      navigate(cancelHref);
    }
  };

  const cloneSource = useMemo(
    () =>
      form.source === "clone" && form.cloneFrom
        ? data.configs.find((c) => c.metadata.name === form.cloneFrom)
        : undefined,
    [data.configs, form.source, form.cloneFrom]
  );

  if (data.error || !data.service) {
    return (
      <div className="flex flex-col gap-4 px-6 py-4">
        <h1 className="text-2xl font-bold">New configuration</h1>
        <Alert variant="destructive">
          <AlertTitle>Couldn't load service</AlertTitle>
          <AlertDescription>
            {data.error ?? "Service not found."}
          </AlertDescription>
        </Alert>
        <a href={cancelHref} className="text-sm text-primary hover:underline">
          ← Back to configurations
        </a>
      </div>
    );
  }

  const service = data.service;
  const subtitle =
    form.source === "clone" && cloneSource?.spec.version
      ? `Based on v${cloneSource.spec.version}`
      : `for ${service.spec.displayName || service.spec.serviceName}`;

  return (
    <div className="grid h-[calc(100vh-3rem)] grid-rows-[auto_1fr]">
      <header className="flex items-center justify-between border-b border-border/50 px-6 py-3">
        <div>
          <h1 className="text-xl font-bold">New configuration</h1>
          <p className="text-sm text-muted-foreground">{subtitle}</p>
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
                <AlertTitle>Couldn't create configuration</AlertTitle>
                <AlertDescription>{actionData.error}</AlertDescription>
              </Alert>
            ) : null}

            {step === 1 ? (
              <Step1VersionSource
                form={form}
                setForm={setForm}
                errors={errors}
                configs={data.configs}
                previousVersion={data.previousVersion}
                suggestedVersion={data.suggestedVersion}
                serviceName={service.spec.serviceName}
              />
            ) : null}
            {step === 2 ? (
              <Step2Mrts
                mrts={form.mrts}
                onChange={(mrts) => setForm({ ...form, mrts })}
                errors={errors}
              />
            ) : null}
            {step === 3 ? (
              <Step3Meters
                meters={form.meters}
                mrts={form.mrts}
                onChange={(meters) => setForm({ ...form, meters })}
                errors={errors}
              />
            ) : null}
            {step === 4 ? (
              <div className="flex flex-col gap-4">
                <Step4Review
                  form={form}
                  service={service}
                  cloneSource={cloneSource}
                />
                <label className="flex items-start gap-3 cursor-pointer max-w-3xl">
                  <Checkbox
                    checked={consented}
                    onCheckedChange={(checked) =>
                      setConsented(checked === true)
                    }
                  />
                  <span className="text-sm text-foreground">
                    I confirm the version and contents are correct.
                  </span>
                </label>
              </div>
            ) : null}
          </main>

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
                    {submitting ? "Creating…" : "Create configuration"}
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
