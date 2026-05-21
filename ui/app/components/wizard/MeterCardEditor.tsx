import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { Input } from "@datum-cloud/datum-ui/input";
import { Label } from "@datum-cloud/datum-ui/label";
import { MultiSelect } from "@datum-cloud/datum-ui/multi-select";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@datum-cloud/datum-ui/select";
import { Textarea } from "@datum-cloud/datum-ui/textarea";
import { Trash2 } from "lucide-react";
import { FieldError } from "./FieldError";
import { AGGREGATIONS, type MeterDraft } from "./wizard-validation";

export function MeterCardEditor({
  index,
  meter,
  mrtOptions,
  errors,
  onChange,
  onRemove,
}: {
  index: number;
  meter: MeterDraft;
  mrtOptions: { value: string; label: string }[];
  errors: Record<string, string>;
  onChange: (next: MeterDraft) => void;
  onRemove: () => void;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pt-4 pb-2">
        <CardTitle className="font-mono text-sm">
          {meter.name || `Meter ${index + 1}`}
        </CardTitle>
        <Button
          type="danger"
          theme="borderless"
          size="icon"
          htmlType="button"
          aria-label="Remove meter"
          onClick={onRemove}
          icon={<Trash2 className="h-4 w-4" />}
        />
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-4 pb-5">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-name`}>Name</Label>
          <Input
            id={`meter-${index}-name`}
            value={meter.name}
            onChange={(e) => onChange({ ...meter, name: e.target.value })}
            placeholder="e.g. cpu-seconds"
          />
          <FieldError message={errors[`meter-${index}-name`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-displayName`}>Display name</Label>
          <Input
            id={`meter-${index}-displayName`}
            value={meter.displayName}
            onChange={(e) =>
              onChange({ ...meter, displayName: e.target.value })
            }
          />
          <FieldError message={errors[`meter-${index}-displayName`]} />
        </div>
        <div className="flex flex-col gap-1.5 col-span-2">
          <Label htmlFor={`meter-${index}-description`}>Description</Label>
          <Textarea
            id={`meter-${index}-description`}
            value={meter.description}
            onChange={(e) =>
              onChange({ ...meter, description: e.target.value })
            }
            rows={2}
            maxLength={500}
          />
          <FieldError message={errors[`meter-${index}-description`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-aggregation`}>Aggregation</Label>
          <Select
            value={meter.measurement.aggregation}
            onValueChange={(value) =>
              onChange({
                ...meter,
                measurement: { ...meter.measurement, aggregation: value },
              })
            }
          >
            <SelectTrigger id={`meter-${index}-aggregation`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {AGGREGATIONS.map((a) => (
                <SelectItem key={a} value={a}>
                  {a}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <FieldError message={errors[`meter-${index}-aggregation`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-unit`}>Measurement unit</Label>
          <Input
            id={`meter-${index}-unit`}
            value={meter.measurement.unit}
            onChange={(e) =>
              onChange({
                ...meter,
                measurement: { ...meter.measurement, unit: e.target.value },
              })
            }
            placeholder="e.g. By, s, {request}"
          />
          <FieldError message={errors[`meter-${index}-unit`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-unitDisplayName`}>
            Unit label{" "}
            <span className="text-muted-foreground font-normal">(optional)</span>
          </Label>
          <Input
            id={`meter-${index}-unitDisplayName`}
            value={meter.measurement.unitDisplayName}
            onChange={(e) =>
              onChange({
                ...meter,
                measurement: {
                  ...meter.measurement,
                  unitDisplayName: e.target.value,
                },
              })
            }
            placeholder="e.g. Byte, Second, Request"
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-consumedUnit`}>
            Billing — consumed unit
          </Label>
          <Input
            id={`meter-${index}-consumedUnit`}
            value={meter.billing.consumedUnit}
            onChange={(e) =>
              onChange({
                ...meter,
                billing: { ...meter.billing, consumedUnit: e.target.value },
              })
            }
            placeholder="e.g. GBy"
          />
          <FieldError message={errors[`meter-${index}-consumedUnit`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-consumedUnitDisplayName`}>
            Consumed unit label{" "}
            <span className="text-muted-foreground font-normal">(optional)</span>
          </Label>
          <Input
            id={`meter-${index}-consumedUnitDisplayName`}
            value={meter.billing.consumedUnitDisplayName}
            onChange={(e) =>
              onChange({
                ...meter,
                billing: {
                  ...meter.billing,
                  consumedUnitDisplayName: e.target.value,
                },
              })
            }
            placeholder="e.g. Gigabyte"
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-pricingUnit`}>
            Billing — pricing unit
          </Label>
          <Input
            id={`meter-${index}-pricingUnit`}
            value={meter.billing.pricingUnit}
            onChange={(e) =>
              onChange({
                ...meter,
                billing: { ...meter.billing, pricingUnit: e.target.value },
              })
            }
            placeholder="e.g. h"
          />
          <FieldError message={errors[`meter-${index}-pricingUnit`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`meter-${index}-pricingUnitDisplayName`}>
            Pricing unit label{" "}
            <span className="text-muted-foreground font-normal">(optional)</span>
          </Label>
          <Input
            id={`meter-${index}-pricingUnitDisplayName`}
            value={meter.billing.pricingUnitDisplayName}
            onChange={(e) =>
              onChange({
                ...meter,
                billing: {
                  ...meter.billing,
                  pricingUnitDisplayName: e.target.value,
                },
              })
            }
            placeholder="e.g. Hour"
          />
        </div>
        <div className="flex flex-col gap-1.5 col-span-2">
          <Label>Bound monitored resources</Label>
          {mrtOptions.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              Add a monitored resource to bind meters to it.
            </p>
          ) : (
            <MultiSelect
              options={mrtOptions}
              value={meter.monitoredResourceTypes}
              onValueChange={(values: string[]) =>
                onChange({ ...meter, monitoredResourceTypes: values })
              }
              placeholder="Select one or more"
            />
          )}
          <FieldError
            message={errors[`meter-${index}-monitoredResourceTypes`]}
          />
        </div>
      </CardContent>
    </Card>
  );
}
