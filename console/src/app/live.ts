// Run events → query refetches. The stream is the tab's one /v1/events
// connection for the current scope; each event refetches what it can change.
// Keys here must match the pages' useQuery/useScopedQuery keys.
import { useEffect } from "react";
import { invalidate, onLiveEvent, useLiveStream, type FeedEvent } from "../api/index.ts";
import { useScope } from "./scope.tsx";

/** Query key prefixes that list runs or count them: any event may change them. */
const RUN_LISTS = ["runs:", "host-runs:", "status@"];

/** Query key prefixes of one Run's data. */
const RUN_DATA = ["run:", "run-events:", "run-snapshots:", "run-artifacts:"];

/** Query key prefixes of one Run's data keyed further (run-server-log:<run>:<name>). */
const RUN_DATA_PREFIX = ["run-server-log:"];

/** Query key prefixes a server's events change: the server lists, and that server's page. */
const SERVER_LISTS = ["servers@", "servers-unattached"];
const SERVER_DATA = ["server:", "server-events:", "server-log:"];

function refetchFor(e: FeedEvent) {
  const run = e.runId;
  invalidate(
    (k) =>
      RUN_LISTS.some((p) => k.startsWith(p)) ||
      (run != null && (RUN_DATA.some((p) => k === p + run) || RUN_DATA_PREFIX.some((p) => k.startsWith(p + run + ":")))) ||
      (e.serverId != null && (SERVER_LISTS.some((p) => k.startsWith(p)) || SERVER_DATA.some((p) => k === p + e.serverId))),
  );
}

/** Open the event stream for the current scope and refetch on its events. Mount once. */
export function useLiveUpdates() {
  const { apiTenant } = useScope();
  useLiveStream(apiTenant);
  useEffect(() => onLiveEvent(refetchFor), []);
}
