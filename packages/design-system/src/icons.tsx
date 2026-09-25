// Small inline icon set (16px grid, stroke 1.5). Kept tiny on purpose.
import type { SVGProps } from "react";

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function Icon({ size = 16, children, ...rest }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...rest}
    >
      {children}
    </svg>
  );
}

export const IconSun = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="8" cy="8" r="3" />
    <path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1 1M11.6 11.6l1 1M3.4 12.6l1-1M11.6 4.4l1-1" />
  </Icon>
);
export const IconMoon = (p: IconProps) => (
  <Icon {...p}>
    <path d="M13.5 9.5A6 6 0 0 1 6.5 2.5a6 6 0 1 0 7 7z" />
  </Icon>
);
export const IconCopy = (p: IconProps) => (
  <Icon {...p}>
    <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" />
    <path d="M10.5 5.5v-2a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2" />
  </Icon>
);
export const IconCheck = (p: IconProps) => (
  <Icon {...p}>
    <path d="M3 8.5l3 3 7-7" />
  </Icon>
);
export const IconChevronDown = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4 6l4 4 4-4" />
  </Icon>
);
export const IconChevronUp = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4 10l4-4 4 4" />
  </Icon>
);
export const IconClose = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4 4l8 8M12 4l-8 8" />
  </Icon>
);
export const IconSearch = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="7" cy="7" r="4.5" />
    <path d="M10.5 10.5L14 14" />
  </Icon>
);
export const IconRefresh = (p: IconProps) => (
  <Icon {...p}>
    <path d="M13.5 8a5.5 5.5 0 1 1-1.6-3.9" />
    <path d="M13.5 2.5v3h-3" />
  </Icon>
);
export const IconArrowDown = (p: IconProps) => (
  <Icon {...p}>
    <path d="M8 2.5v11M3.5 9l4.5 4.5L12.5 9" />
  </Icon>
);
export const IconDots = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="3.5" cy="8" r="1" fill="currentColor" stroke="none" />
    <circle cx="8" cy="8" r="1" fill="currentColor" stroke="none" />
    <circle cx="12.5" cy="8" r="1" fill="currentColor" stroke="none" />
  </Icon>
);
export const IconWarning = (p: IconProps) => (
  <Icon {...p}>
    <path d="M8 2.5l6 11H2z" />
    <path d="M8 6.5v3M8 11.5v.5" />
  </Icon>
);
export const IconInfo = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="8" cy="8" r="6" />
    <path d="M8 7.5v4M8 5v.5" />
  </Icon>
);
export const IconInbox = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2 9l1.5-5h9L14 9v4H2z" />
    <path d="M2 9h3.5l1 2h3l1-2H14" />
  </Icon>
);
export const IconClock = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="8" cy="8" r="6" />
    <path d="M8 4.5V8l2.5 1.5" />
  </Icon>
);
export const IconGrid = (p: IconProps) => (
  <Icon {...p}>
    <rect x="2.5" y="2.5" width="4.5" height="4.5" rx="1" />
    <rect x="9" y="2.5" width="4.5" height="4.5" rx="1" />
    <rect x="2.5" y="9" width="4.5" height="4.5" rx="1" />
    <rect x="9" y="9" width="4.5" height="4.5" rx="1" />
  </Icon>
);
export const IconPlay = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4.5 3l8 5-8 5z" />
  </Icon>
);
export const IconServer = (p: IconProps) => (
  <Icon {...p}>
    <rect x="2.5" y="3" width="11" height="4" rx="1" />
    <rect x="2.5" y="9" width="11" height="4" rx="1" />
    <path d="M5 5h.01M5 11h.01" />
  </Icon>
);
export const IconLayers = (p: IconProps) => (
  <Icon {...p}>
    <path d="M8 2.5l6 3-6 3-6-3z" />
    <path d="M2 8.5l6 3 6-3M2 11l6 3 6-3" />
  </Icon>
);
export const IconUsers = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="6" cy="5.5" r="2.5" />
    <path d="M1.5 13.5a4.5 4.5 0 0 1 9 0" />
    <path d="M10.5 3.2a2.5 2.5 0 0 1 0 4.6M12 9.3a4.5 4.5 0 0 1 2.5 4.2" />
  </Icon>
);
export const IconLogout = (p: IconProps) => (
  <Icon {...p}>
    <path d="M6.5 2.5H3.5a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h3" />
    <path d="M10 5l3 3-3 3M13 8H6" />
  </Icon>
);
export const IconDownload = (p: IconProps) => (
  <Icon {...p}>
    <path d="M8 2.5v8M4.5 7l3.5 3.5L11.5 7" />
    <path d="M2.5 13.5h11" />
  </Icon>
);
export const IconSend = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2.5 8l11-5.5-3 11-2.5-4z" />
    <path d="M8 9.5l5.5-7" />
  </Icon>
);
export const IconMenu = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11" />
  </Icon>
);
export const IconSliders = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2.5 4.5h7M12.5 4.5h1M2.5 11.5h2M7.5 11.5h6" />
    <circle cx="11" cy="4.5" r="1.5" />
    <circle cx="6" cy="11.5" r="1.5" />
  </Icon>
);
export const IconRows = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2.5 3.5h11M2.5 6.5h11M2.5 9.5h11M2.5 12.5h11" />
  </Icon>
);
export const IconRowsLoose = (p: IconProps) => (
  <Icon {...p}>
    <path d="M2.5 4h11M2.5 8h11M2.5 12h11" />
  </Icon>
);
export const IconSidebar = (p: IconProps) => (
  <Icon {...p}>
    <rect x="2" y="3" width="12" height="10" rx="1.5" />
    <path d="M6 3v10" />
  </Icon>
);
export const IconPalette = (p: IconProps) => (
  <Icon {...p}>
    <path d="M8 2a6 6 0 1 0 0 12c1 0 1.5-.6 1.5-1.3 0-.8-.6-1-.6-1.7 0-.6.5-1 1.1-1H11a3 3 0 0 0 3-3c0-2.8-2.7-5-6-5z" />
    <circle cx="5" cy="7" r=".8" fill="currentColor" stroke="none" />
    <circle cx="8" cy="5" r=".8" fill="currentColor" stroke="none" />
    <circle cx="11" cy="7" r=".8" fill="currentColor" stroke="none" />
  </Icon>
);
