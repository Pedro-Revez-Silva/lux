import { IconClock } from "./icons.tsx";
import { Select } from "./Select.tsx";

export type TimeRange = "1h" | "6h" | "24h" | "7d" | "30d";

export const TIME_RANGES: { value: TimeRange; label: string }[] = [
  { value: "1h", label: "Last hour" },
  { value: "6h", label: "Last 6 hours" },
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
];

export interface TimeRangePickerProps {
  value: TimeRange;
  onChange: (r: TimeRange) => void;
  size?: "sm" | "md";
}

export function TimeRangePicker({ value, onChange, size = "sm" }: TimeRangePickerProps) {
  return (
    <Select
      options={TIME_RANGES.map((t) => ({ value: t.value, label: t.label, text: t.value }))}
      value={value}
      onChange={onChange}
      icon={<IconClock size={13} />}
      size={size}
      width={92}
    />
  );
}
