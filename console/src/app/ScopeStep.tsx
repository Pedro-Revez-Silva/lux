import { StepPicker } from "@lux/design-system";
import { autoStep, autoSummary, everyOptions, stepText } from "./every.ts";
import { useScope } from "./scope.tsx";

/** The top bar's Every: the step of every chart that follows the page's range. */
export function ScopeStep() {
  const scope = useScope();
  return (
    <StepPicker
      options={everyOptions(scope.range)}
      value={scope.everyInEffect}
      onChange={scope.setEvery}
      resolved={autoSummary(scope.range)}
      note={
        <>
          <strong>Auto</strong> for {scope.range}: trends {stepText(autoStep(scope.range, "trend"))}, cost {stepText(autoStep(scope.range, "cost"))}. A fixed choice applies to every chart that follows the range. Cost is never finer than an hour: with Minute, cost charts stay hourly and say so.
        </>
      }
    />
  );
}
