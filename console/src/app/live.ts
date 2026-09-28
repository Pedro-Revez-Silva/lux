// Run events → query refetches. The stream is the tab's one /v1/events
// connection for the current scope; each event refetches what it can change.
// Keys here must match the pages' useQuery/useScopedQuery keys.
import { useEffect } from "react";
import { invalidate, onLiveEvent, useLiveStream, type FeedEvent } from "../api/index.ts";
import { useScope } from "./scope.tsx";

/** Query key prefixes that list runs or count them: any event may change them. */
const RUN_LISTS = ["runs:", "host-runs:", "status@"];

/** Query key prefixes of one Run's data. */
const RUN_DATA = ["run:", "run-events:", "run-snapshots:", "run-artifacts:", "run-servers:"];

function refetchFor(e: FeedEvent) {
  invalidate((k) => RUN_LISTS.some((p) => k.startsWith(p)) || RUN_DATA.some((p) => k === p + e.runId));
}

/** Open the event stream for the current scope and refetch on its events. Mount once. */
export function useLiveUpdates() {
  const { apiTenant } = useScope();
  useLiveStream(apiTenant);
  useEffect(() => onLiveEvent(refetchFor), []);
}
