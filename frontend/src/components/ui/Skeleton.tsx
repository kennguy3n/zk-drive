import { cn } from "../../lib/cn";

// Skeleton is a shimmering placeholder block. The `.skeleton` class
// (defined in index.css) carries the animated gradient so it themes with
// the rest of the app. Width/height come from utility classes or style.
export function Skeleton({
  className,
  style,
}: {
  className?: string;
  style?: React.CSSProperties;
}) {
  return (
    <div
      className={cn("skeleton", className)}
      style={style}
      aria-hidden="true"
    />
  );
}

// FileListSkeleton mimics the shape of a populated file list so the
// layout doesn't jump when real rows arrive (CLS = 0).
export function FileListSkeleton({ rows = 8 }: { rows?: number }) {
  return (
    <div
      className="flex flex-col gap-2 p-2"
      role="status"
      aria-label="Loading files"
    >
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-3 px-2 py-2">
          <Skeleton className="h-9 w-9 shrink-0 rounded-lg" />
          <Skeleton className="h-4" style={{ width: `${40 + ((i * 7) % 40)}%` }} />
          <Skeleton className="ml-auto h-4 w-16" />
        </div>
      ))}
    </div>
  );
}

// FolderTreeSkeleton mimics an indented folder tree.
export function FolderTreeSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div
      className="flex flex-col gap-2 p-2"
      role="status"
      aria-label="Loading folders"
    >
      {Array.from({ length: rows }).map((_, i) => (
        <div
          key={i}
          className="flex items-center gap-2"
          style={{ paddingLeft: `${(i % 3) * 16}px` }}
        >
          <Skeleton className="h-4 w-4 rounded" />
          <Skeleton className="h-4" style={{ width: `${50 + ((i * 11) % 30)}%` }} />
        </div>
      ))}
    </div>
  );
}

// PagePreviewSkeleton is a generic full-panel placeholder used as the
// route-level Suspense fallback (replaces the plain "Loading…" text).
export function PagePreviewSkeleton() {
  return (
    <div className="mx-auto w-full max-w-5xl p-6" role="status" aria-label="Loading">
      <Skeleton className="mb-4 h-7 w-48" />
      <Skeleton className="mb-6 h-4 w-72" />
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-28 rounded-card" />
        ))}
      </div>
    </div>
  );
}
