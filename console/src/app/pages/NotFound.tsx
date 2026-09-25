import { EmptyState } from "@lux/design-system";

export function NotFound({ path }: { path: string }) {
  return (
    <div className="page">
      <EmptyState title="Not found" description={`No page at ${path}.`} />
    </div>
  );
}
