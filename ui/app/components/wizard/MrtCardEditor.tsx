import { Button } from "@datum-cloud/datum-ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@datum-cloud/datum-ui/card";
import { Input } from "@datum-cloud/datum-ui/input";
import { Label } from "@datum-cloud/datum-ui/label";
import { TagsInput } from "@datum-cloud/datum-ui/tag-input";
import { Textarea } from "@datum-cloud/datum-ui/textarea";
import { Trash2 } from "lucide-react";
import { FieldError } from "./FieldError";
import type { MrtDraft } from "./wizard-validation";

export function MrtCardEditor({
  index,
  mrt,
  errors,
  onChange,
  onRemove,
}: {
  index: number;
  mrt: MrtDraft;
  errors: Record<string, string>;
  onChange: (next: MrtDraft) => void;
  onRemove: () => void;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pt-4 pb-2">
        <CardTitle className="font-mono text-sm">
          {mrt.type || `Resource type ${index + 1}`}
        </CardTitle>
        <Button
          type="danger"
          theme="borderless"
          size="icon"
          htmlType="button"
          aria-label="Remove resource type"
          onClick={onRemove}
          icon={<Trash2 className="h-4 w-4" />}
        />
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-4 pb-5">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`mrt-${index}-type`}>Type</Label>
          <Input
            id={`mrt-${index}-type`}
            value={mrt.type}
            onChange={(e) => onChange({ ...mrt, type: e.target.value })}
            placeholder="e.g. compute-instance"
          />
          <FieldError message={errors[`mrt-${index}-type`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`mrt-${index}-displayName`}>Display name</Label>
          <Input
            id={`mrt-${index}-displayName`}
            value={mrt.displayName}
            onChange={(e) =>
              onChange({ ...mrt, displayName: e.target.value })
            }
          />
          <FieldError message={errors[`mrt-${index}-displayName`]} />
        </div>
        <div className="flex flex-col gap-1.5 col-span-2">
          <Label htmlFor={`mrt-${index}-description`}>Description</Label>
          <Textarea
            id={`mrt-${index}-description`}
            value={mrt.description}
            onChange={(e) =>
              onChange({ ...mrt, description: e.target.value })
            }
            rows={2}
            maxLength={500}
          />
          <FieldError message={errors[`mrt-${index}-description`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`mrt-${index}-group`}>GVK group</Label>
          <Input
            id={`mrt-${index}-group`}
            value={mrt.gvk.group}
            onChange={(e) =>
              onChange({
                ...mrt,
                gvk: { ...mrt.gvk, group: e.target.value },
              })
            }
            placeholder="e.g. compute.miloapis.com"
          />
          <FieldError message={errors[`mrt-${index}-group`]} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`mrt-${index}-kind`}>GVK kind</Label>
          <Input
            id={`mrt-${index}-kind`}
            value={mrt.gvk.kind}
            onChange={(e) =>
              onChange({
                ...mrt,
                gvk: { ...mrt.gvk, kind: e.target.value },
              })
            }
            placeholder="e.g. Instance"
          />
          <FieldError message={errors[`mrt-${index}-kind`]} />
        </div>
        <div className="flex flex-col gap-1.5 col-span-2">
          <Label>Labels</Label>
          <TagsInput
            value={mrt.labels}
            onValueChange={(values) =>
              onChange({ ...mrt, labels: values })
            }
            placeholder="Add a label and press Enter"
          />
        </div>
      </CardContent>
    </Card>
  );
}
