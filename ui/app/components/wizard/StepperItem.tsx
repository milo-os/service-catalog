import { Check } from "lucide-react";

export function StepperItem({
  n,
  label,
  active,
  done,
}: {
  n: number;
  label: string;
  active: boolean;
  done: boolean;
}) {
  const circleClass = done
    ? "bg-success-100 text-success-700 border-success-300"
    : active
      ? "bg-primary text-primary-foreground border-primary"
      : "bg-muted text-muted-foreground border-border";
  return (
    <li className="flex items-center gap-3 py-1.5">
      <div
        className={`flex h-7 w-7 items-center justify-center rounded-full border text-xs font-semibold ${circleClass}`}
      >
        {done ? <Check className="h-4 w-4" /> : n}
      </div>
      <span
        className={
          active
            ? "text-sm font-medium text-foreground"
            : done
              ? "text-sm text-muted-foreground line-through"
              : "text-sm text-muted-foreground"
        }
      >
        {label}
      </span>
    </li>
  );
}
