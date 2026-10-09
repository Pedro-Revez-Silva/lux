import type { ReactNode } from "react";
import { Select, type SelectOption } from "./Select.tsx";

export type StepChoice = "auto" | "minute" | "hour" | "day";

export interface StepPickerOption {
  value: StepChoice;
  /** On the right: the points it gives ("1,440 points"), or what Auto does ("each chart picks"). */
  hint?: string;
  /** Why it cannot be picked; the option is greyed. */
  disabled?: string;
}

export interface StepPickerProps {
  options: StepPickerOption[];
  value: StepChoice;
  onChange: (v: StepChoice) => void;
  /** After the value in the trigger, muted: what Auto resolved to ("min/hour"). */
  resolved?: string;
  /** Under the options: what the choice does to each kind of chart. */
  note?: ReactNode;
  size?: "sm" | "md";
}

const LABEL: Record<StepChoice, string> = { auto: "Auto", minute: "Minute", hour: "Hour", day: "Day" };

/**
 * The page's step beside the TimeRangePicker: "Every Auto · min/hour".
 * Auto lets each chart pick its natural step for the range; a fixed choice
 * applies to every chart that follows the range. Choices the range cannot
 * use are greyed with the reason.
 */
export function StepPicker({ options, value, onChange, resolved, note, size = "sm" }: StepPickerProps) {
  // The trigger shows the current option's text: Auto carries what it resolved to.
  const trigger = value === "auto" && resolved ? `${LABEL.auto} · ${resolved}` : LABEL[value];
  const opts: SelectOption<StepChoice>[] = options.map((o) => ({
    value: o.value,
    text: o.value === value ? trigger : LABEL[o.value],
    label: LABEL[o.value],
    hint: o.hint,
    disabled: o.disabled,
  }));
  return <Select className="step-picker" options={opts} value={value} onChange={onChange} prefix="Every" size={size} footer={note} />;
}
