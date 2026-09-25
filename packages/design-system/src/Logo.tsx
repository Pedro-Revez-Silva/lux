// The lux mark: a four-point star, gold with a warm and a light facet and
// a navy outline. The same drawing as docs/brand/lux-small.svg, made for
// 16–32px; docs/brand/lux.svg is the detailed one for larger uses.
export function Logo({ size = 18, className }: { size?: number; className?: string }) {
  return (
    <svg className={className} width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" focusable="false">
      <path d="M16 1.5Q17.3 14.7 30.5 16Q17.3 17.3 16 30.5Q14.7 17.3 1.5 16Q14.7 14.7 16 1.5Z" fill="#ffd166" stroke="#163f7a" strokeWidth="1.25" strokeLinejoin="round" />
      <path d="M16 3Q14.8 14.8 3 16L16 16Z" fill="#fff4cc" />
      <path d="M29 16Q17.2 17.2 16 29L16 16Z" fill="#ff9f43" />
    </svg>
  );
}
