import type { ReactElement } from "react";
import { StepPicker } from "@lux/design-system";
import { autoStep, everyOptions, stepText } from "./every.ts";
import { useScope } from "./scope.tsx";

/** The top bar's Every: the step of every chart that follows the page's range. */
export function ScopeStep(): ReactElement {
  const scope = useScope();
  return (
    <StepPicker
      options={everyOptions(scope.range)}
      value={scope.everyInEffect}
      onChange={scope.setEvery}
      resolved={`server picks trends / cost ${autoStep(scope.range, "cost")}`}
      note={
        <>
          <strong>Auto</strong>: the server picks trends from the subject's available lifetime; cost for {scope.range} is {stepText(autoStep(scope.range, "cost"))}. A fixed choice applies to every chart that follows the range. Cost is never finer than an hour: with Minute, cost charts stay hourly and say so.
        </>
      }
    />
  );
}
