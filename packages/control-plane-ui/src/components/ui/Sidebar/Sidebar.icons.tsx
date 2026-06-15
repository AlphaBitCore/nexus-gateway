/**
 * Sidebar SVG icon glyphs — PURELY PRESENTATIONAL (icon-name → SVG path).
 * Extracted from Sidebar.tsx so the nav/permission logic stays small and
 * testable while this icon-map data (like locale JSON) is excluded from
 * coverage — asserting an icon name maps to an SVG path is test-for-test
 * padding, not a business behaviour.
 */
import { type SVGProps } from 'react';
import clsx from 'clsx';
import styles from './Sidebar.module.css';

function Icon(props: SVGProps<SVGSVGElement>) {
  const { className, children, ...rest } = props;
  return (
    <svg
      className={clsx(styles.navIcon, className)}
      width={16}
      height={16}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      {...rest}
    >
      {children}
    </svg>
  );
}

export function MenuIcon({ name }: { name: string }) {
  switch (name) {
    case 'bell':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        </Icon>
      );
    case 'sun':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
        </Icon>
      );
    case 'moon':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79Z" />
        </Icon>
      );
    case 'monitor':
      return (
        <Icon className={styles.menuIcon}>
          <rect x="2" y="3" width="20" height="14" rx="2" />
          <path d="M8 21h8M12 17v4" />
        </Icon>
      );
    case 'palette':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="13.5" cy="6.5" r="2.5" />
          <circle cx="17.5" cy="10.5" r="1.5" />
          <circle cx="8.5" cy="7.5" r="1.5" />
          <circle cx="6.5" cy="12.5" r="1.5" />
          <path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.93 0 1.65-.75 1.65-1.69 0-.44-.18-.84-.44-1.13-.29-.29-.44-.65-.44-1.13 0-.92.75-1.66 1.67-1.66h2c3.05 0 5.56-2.5 5.56-5.55C21.97 6.01 17.46 2 12 2Z" />
        </Icon>
      );
    case 'globe':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="10" />
          <path d="M2 12h20M12 2a15 15 0 0 1 4 10 15 15 0 0 1-4 10 15 15 0 0 1-4-10 15 15 0 0 1 4-10Z" />
        </Icon>
      );
    case 'user':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="8" r="4" />
          <path d="M4 21c0-4 4-7 8-7s8 3 8 7" />
        </Icon>
      );
    case 'logout':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
          <path d="M16 17l5-5-5-5M21 12H9" />
        </Icon>
      );
    default:
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="4" />
        </Icon>
      );
  }
}

type NavIconName =
  | 'activity'
  | 'alert-triangle'
  | 'archive'
  | 'bell'
  | 'bot'
  | 'bookmark'
  | 'bolt'
  | 'box'
  | 'broadcast'
  | 'bug'
  | 'building'
  | 'chart'
  | 'circle-check'
  | 'clipboard'
  | 'clock'
  | 'cog'
  | 'cube'
  | 'database'
  | 'deviceSquare'
  | 'domain'
  | 'dot'
  | 'file'
  | 'fileCheck'
  | 'fileMinus'
  | 'fileSearch'
  | 'folder'
  | 'folderUsers'
  | 'gauge'
  | 'globe'
  | 'grid'
  | 'hard-drive'
  | 'hourglass'
  | 'id-card'
  | 'key'
  | 'key-2'
  | 'layers'
  | 'list-check'
  | 'link'
  | 'lock-keyhole'
  | 'monitor'
  | 'network'
  | 'radio'
  | 'pencil'
  | 'play'
  | 'power'
  | 'radar'
  | 'refresh'
  | 'route'
  | 'server'
  | 'terminal'
  | 'timer'
  | 'upload-cloud'
  | 'wrench'
  | 'wand'
  | 'sliders'
  | 'scan'
  | 'shield'
  | 'shield-check'
  | 'users';

function SidebarIconGlyph({ name }: { name: NavIconName }) {
  switch (name) {
    case 'grid':
      return (
        <Icon>
          <rect x="3" y="3" width="7" height="7" rx="1" />
          <rect x="14" y="3" width="7" height="7" rx="1" />
          <rect x="3" y="14" width="7" height="7" rx="1" />
          <rect x="14" y="14" width="7" height="7" rx="1" />
        </Icon>
      );
    case 'activity':
      return (
        <Icon>
          <path d="M3 12l4-4 4 6 4-2 6 4" />
        </Icon>
      );
    case 'bell':
      return (
        <Icon>
          <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        </Icon>
      );
    case 'bot':
      return (
        <Icon>
          <rect x="5" y="7" width="14" height="12" rx="2" />
          <path d="M12 7V3" />
          <path d="M8 3h8" />
          <path d="M9 12h.01M15 12h.01" />
          <path d="M9 16h6" />
        </Icon>
      );
    case 'alert-triangle':
      return (
        <Icon>
          <path d="M10.3 3.7 2.4 18a2 2 0 0 0 1.8 3h15.6a2 2 0 0 0 1.8-3L13.7 3.7a2 2 0 0 0-3.4 0Z" />
          <path d="M12 9v4" />
          <path d="M12 17h.01" />
        </Icon>
      );
    case 'archive':
      return (
        <Icon>
          <rect x="3" y="4" width="18" height="4" rx="1" />
          <path d="M5 8v11a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8" />
          <path d="M10 12h4" />
        </Icon>
      );
    case 'box':
      return (
        <Icon>
          <path d="M21 8 12 3 3 8l9 5 9-5Z" />
          <path d="M3 8v8l9 5 9-5V8" />
          <path d="M12 13v8" />
        </Icon>
      );
    case 'broadcast':
      return (
        <Icon>
          <path d="M4.5 16.5a7 7 0 0 1 0-9" />
          <path d="M19.5 7.5a7 7 0 0 1 0 9" />
          <path d="M8 13a3 3 0 0 1 0-2" />
          <path d="M16 11a3 3 0 0 1 0 2" />
          <circle cx="12" cy="12" r="1" />
        </Icon>
      );
    case 'bug':
      return (
        <Icon>
          <path d="M8 2l1.88 1.88M16 2l-1.88 1.88" />
          <rect x="7" y="5" width="10" height="14" rx="5" />
          <path d="M3 13h4M17 13h4M4 19l3-3M20 19l-3-3M4 7l3 3M20 7l-3 3" />
        </Icon>
      );
    case 'chart':
      return (
        <Icon>
          <path d="M3 3v18h18" />
          <path d="M8 17V9" />
          <path d="M13 17v-5" />
          <path d="M18 17V5" />
        </Icon>
      );
    case 'clock':
      return (
        <Icon>
          <circle cx="12" cy="12" r="9" />
          <path d="M12 7v5l3 2" />
        </Icon>
      );
    case 'layers':
      return (
        <Icon>
          <path d="M3 7l9-4 9 4-9 4-9-4z" />
          <path d="M3 12l9 4 9-4" />
          <path d="M3 17l9 4 9-4" />
        </Icon>
      );
    case 'cog':
      return (
        <Icon>
          <circle cx="12" cy="12" r="3" />
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
        </Icon>
      );
    case 'key':
      return (
        <Icon>
          <circle cx="8" cy="15" r="4" />
          <path d="M11 12l9-9 3 3-3 3 2 2-3 3-2-2-2 2-3-3" />
        </Icon>
      );
    case 'key-2':
      return (
        <Icon>
          <path d="M21 2l-2 2-3-3-7 7 5 5 7-7-3-3z" />
          <path d="M3 21h18" />
        </Icon>
      );
    case 'shield-check':
      return (
        <Icon>
          <path d="M12 2l8 4v6c0 5-3.5 9-8 10-4.5-1-8-5-8-10V6z" />
          <path d="M9 12l2 2 4-4" />
        </Icon>
      );
    case 'route':
      return (
        <Icon>
          <path d="M9 17h6" />
          <path d="M3 7h18" />
          <path d="M5 7v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V7" />
          <path d="M12 11v6" />
        </Icon>
      );
    case 'circle-check':
      return (
        <Icon>
          <circle cx="12" cy="12" r="10" />
          <path d="M9 12l2 2 4-4" />
        </Icon>
      );
    case 'clipboard':
      return (
        <Icon>
          <rect x="8" y="2" width="8" height="4" rx="1" />
          <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
          <path d="M8 12h8M8 16h5" />
        </Icon>
      );
    case 'domain':
      return (
        <Icon>
          <rect x="3" y="8" width="18" height="12" rx="2" />
          <path d="M7 8V5a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v3" />
          <path d="M7 13h2M11 13h2M15 13h2M7 17h2M11 17h2M15 17h2" />
        </Icon>
      );
    case 'fileSearch':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h6" />
          <polyline points="14 2 14 8 20 8" />
          <circle cx="16" cy="16" r="3" />
          <path d="m18.5 18.5 2.5 2.5" />
        </Icon>
      );
    case 'fileMinus':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
          <path d="M8 15h8" />
        </Icon>
      );
    case 'folderUsers':
      return (
        <Icon>
          <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
          <circle cx="10" cy="12" r="2" />
          <path d="M6.5 17a3.5 3.5 0 0 1 7 0" />
          <path d="M15 11.5a2 2 0 0 1 2 2" />
        </Icon>
      );
    case 'gauge':
      return (
        <Icon>
          <path d="M21 12a9 9 0 1 0-18 0" />
          <path d="M12 12l4-4" />
          <path d="M7 14h10" />
        </Icon>
      );
    case 'bookmark':
      return (
        <Icon>
          <path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z" />
        </Icon>
      );
    case 'file':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
        </Icon>
      );
    case 'bolt':
      return (
        <Icon>
          <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />
        </Icon>
      );
    case 'globe':
      return (
        <Icon>
          <circle cx="12" cy="12" r="10" />
          <line x1="2" y1="12" x2="22" y2="12" />
          <path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z" />
        </Icon>
      );
    case 'dot':
      return (
        <Icon>
          <circle cx="12" cy="12" r="5" />
        </Icon>
      );
    case 'link':
      return (
        <Icon>
          <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
          <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
        </Icon>
      );
    case 'cube':
      return (
        <Icon>
          <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
          <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
          <line x1="12" y1="22.08" x2="12" y2="12" />
        </Icon>
      );
    case 'deviceSquare':
      return (
        <Icon>
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <rect x="8" y="8" width="8" height="8" rx="1" />
        </Icon>
      );
    case 'database':
      return (
        <Icon>
          <ellipse cx="12" cy="5" rx="8" ry="3" />
          <path d="M4 5v6c0 1.66 3.58 3 8 3s8-1.34 8-3V5" />
          <path d="M4 11v6c0 1.66 3.58 3 8 3s8-1.34 8-3v-6" />
        </Icon>
      );
    case 'hard-drive':
      return (
        <Icon>
          <rect x="3" y="5" width="18" height="14" rx="2" />
          <path d="M7 15h.01M11 15h6" />
          <path d="M7 9h10" />
        </Icon>
      );
    case 'id-card':
      return (
        <Icon>
          <rect x="3" y="5" width="18" height="14" rx="2" />
          <circle cx="9" cy="12" r="2" />
          <path d="M13 10h5M13 14h4M6 17a3 3 0 0 1 6 0" />
        </Icon>
      );
    case 'hourglass':
      return (
        <Icon>
          <path d="M6 2h12" />
          <path d="M6 22h12" />
          <path d="M8 2v5a4 4 0 0 0 2 3.46L12 12l2-1.54A4 4 0 0 0 16 7V2" />
          <path d="M8 22v-5a4 4 0 0 1 2-3.46L12 12l2 1.54A4 4 0 0 1 16 17v5" />
        </Icon>
      );
    case 'list-check':
      return (
        <Icon>
          <path d="M9 6h11M9 12h11M9 18h11" />
          <path d="m3 6 1 1 2-2M3 12l1 1 2-2M3 18l1 1 2-2" />
        </Icon>
      );
    case 'lock-keyhole':
      return (
        <Icon>
          <rect x="4" y="11" width="16" height="10" rx="2" />
          <path d="M8 11V7a4 4 0 0 1 8 0v4" />
          <circle cx="12" cy="16" r="1" />
          <path d="M12 17v2" />
        </Icon>
      );
    case 'network':
      return (
        <Icon>
          <rect x="3" y="3" width="7" height="7" rx="1" />
          <rect x="14" y="14" width="7" height="7" rx="1" />
          <path d="M10 6h4a4 4 0 0 1 4 4v4" />
          <path d="M6.5 10v4a4 4 0 0 0 4 4H14" />
        </Icon>
      );
    case 'server':
      return (
        <Icon>
          <rect x="2" y="3" width="20" height="8" rx="2" />
          <rect x="2" y="13" width="20" height="8" rx="2" />
          <line x1="6" y1="7" x2="6.01" y2="7" />
          <line x1="6" y1="17" x2="6.01" y2="17" />
        </Icon>
      );
    case 'refresh':
      return (
        <Icon>
          <polyline points="23 4 23 10 17 10" />
          <polyline points="1 20 1 14 7 14" />
          <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10" />
          <path d="M20.49 15a9 9 0 0 1-14.85 3.36L1 14" />
        </Icon>
      );
    case 'pencil':
      return (
        <Icon>
          <path d="M12 20h9" />
          <path d="M16.5 3.5a2.121 2.121 0 1 1 3 3L7 19l-4 1 1-4z" />
        </Icon>
      );
    case 'power':
      return (
        <Icon>
          <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
          <line x1="12" y1="2" x2="12" y2="12" />
        </Icon>
      );
    case 'folder':
      return (
        <Icon>
          <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
        </Icon>
      );
    case 'play':
      return (
        <Icon>
          <polygon points="6 3 20 12 6 21 6 3" />
        </Icon>
      );
    case 'fileCheck':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
          <polyline points="9 14 11 16 15 12" />
        </Icon>
      );
    case 'users':
      return (
        <Icon>
          <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
          <circle cx="9" cy="7" r="4" />
          <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
          <path d="M16 3.13a4 4 0 0 1 0 7.75" />
        </Icon>
      );
    case 'building':
      return (
        <Icon>
          <rect x="4" y="3" width="16" height="18" rx="1" />
          <path d="M9 8h2M9 12h2M9 16h2M14 8h2M14 12h2M14 16h2" />
        </Icon>
      );
    case 'monitor':
      return (
        <Icon>
          <rect x="2" y="3" width="20" height="14" rx="2" />
          <line x1="8" y1="21" x2="16" y2="21" />
          <line x1="12" y1="17" x2="12" y2="21" />
        </Icon>
      );
    case 'radio':
      return (
        <Icon>
          <path d="M4.9 19.1a10 10 0 0 1 0-14.2" />
          <path d="M7.8 16.2a6 6 0 0 1 0-8.4" />
          <circle cx="12" cy="12" r="2" />
          <path d="M16.2 7.8a6 6 0 0 1 0 8.4" />
          <path d="M19.1 4.9a10 10 0 0 1 0 14.2" />
        </Icon>
      );
    case 'radar':
      return (
        <Icon>
          <circle cx="12" cy="12" r="9" />
          <path d="M12 12l6-6" />
          <path d="M7.8 16.2a6 6 0 0 0 8.4 0" />
          <path d="M9.9 14.1a3 3 0 0 0 4.2 0" />
        </Icon>
      );
    case 'scan':
      return (
        <Icon>
          <path d="M7 3H5a2 2 0 0 0-2 2v2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2" />
          <path d="M8 12h8" />
          <path d="M12 8v8" />
        </Icon>
      );
    case 'shield':
      return (
        <Icon>
          <path d="M12 2l8 4v6c0 5-3.5 9-8 10-4.5-1-8-5-8-10V6z" />
        </Icon>
      );
    case 'sliders':
      return (
        <Icon>
          <path d="M4 6h10M18 6h2M4 12h2M10 12h10M4 18h12M20 18h0" />
          <circle cx="16" cy="6" r="2" />
          <circle cx="8" cy="12" r="2" />
          <circle cx="18" cy="18" r="2" />
        </Icon>
      );
    case 'terminal':
      return (
        <Icon>
          <rect x="3" y="4" width="18" height="16" rx="2" />
          <path d="m8 9 3 3-3 3" />
          <path d="M13 15h4" />
        </Icon>
      );
    case 'timer':
      return (
        <Icon>
          <path d="M10 2h4" />
          <path d="M12 14l3-3" />
          <circle cx="12" cy="14" r="8" />
        </Icon>
      );
    case 'upload-cloud':
      return (
        <Icon>
          <path d="M16 16l-4-4-4 4" />
          <path d="M12 12v9" />
          <path d="M20.4 18.4A5 5 0 0 0 18 9h-1.3A8 8 0 1 0 4 16.3" />
        </Icon>
      );
    case 'wrench':
      return (
        <Icon>
          <path d="M14.7 6.3a4 4 0 0 0-5 5L3 18l3 3 6.7-6.7a4 4 0 0 0 5-5l-2.4 2.4-3-3 2.4-2.4Z" />
        </Icon>
      );
    case 'wand':
      return (
        <Icon>
          <path d="M15 4V2M15 8v-2M11 6h2M17 6h2" />
          <path d="M5 21l14-14-2-2L3 19l2 2Z" />
        </Icon>
      );
  }
  return null;
}

function navIconNameForPath(path: string): NavIconName {
  switch (path) {
    case '/':
      return 'grid';
    case '/traffic':
      return 'activity';
    case '/status':
      return 'gauge';
    case '/analytics':
      return 'chart';
    case '/quota-usage':
      return 'clock';
    case '/infrastructure/jobs':
      return 'timer';
    case '/cache-roi':
      return 'layers';
    case '/iam/roles':
      return 'id-card';
    case '/ai-gateway/providers':
      return 'cog';
    case '/ai-gateway/credentials':
      return 'key';
    case '/ai-gateway/credential-reliability':
      return 'shield-check';
    case '/ai-gateway/routing':
      return 'route';
    case '/ai-gateway/virtual-keys':
      return 'key-2';
    case '/ai-gateway/quota-policies':
      return 'circle-check';
    case '/ai-gateway/quota-overrides':
      return 'bookmark';
    case '/ai-gateway/cache':
      return 'file';
    case '/iam/policies':
      return 'fileCheck';
    case '/iam/oauth-clients':
      return 'cube';
    case '/ai-gateway/passthrough':
      return 'bolt';
    case '/compliance/overview':
      return 'shield';
    case '/compliance/hooks':
      return 'link';
    case '/compliance/rule-packs':
      return 'box';
    case '/compliance/interception-domains':
      return 'domain';
    case '/compliance/exemptions':
      return 'fileMinus';
    case '/compliance/ai-guard':
      return 'scan';
    case '/compliance/streaming':
      return 'radio';
    case '/compliance/payload-capture':
      return 'archive';
    case '/compliance/audit-logs':
      return 'clipboard';
    case '/compliance/dsar':
      return 'fileSearch';
    case '/alerts':
      return 'bell';
    case '/alerts/rules':
      return 'list-check';
    case '/alerts/channels':
      return 'broadcast';
    case '/devices':
      return 'monitor';
    case '/devices/groups':
      return 'folderUsers';
    case '/devices/device-auth':
      return 'lock-keyhole';
    case '/devices/device-defaults':
      return 'deviceSquare';
    case '/infrastructure/nodes':
      return 'server';
    case '/infrastructure/config-sync':
      return 'refresh';
    case '/infrastructure/overrides':
      return 'pencil';
    case '/infrastructure/errors':
      return 'alert-triangle';
    case '/infrastructure/dlq':
      return 'database';
    case '/infrastructure/crashes':
      return 'bug';
    case '/infrastructure/diag-mode':
      return 'wrench';
    case '/infrastructure/observability-config':
      return 'sliders';
    case '/infrastructure/observability-retention':
      return 'hourglass';
    case '/infrastructure/siem':
      return 'radar';
    case '/infrastructure/proxy-rollout':
      return 'network';
    case '/infrastructure/agent-setup':
      return 'hard-drive';
    case '/infrastructure/cli-setup':
      return 'terminal';
    case '/infrastructure/kill-switch':
      return 'power';
    case '/iam/organizations':
      return 'building';
    case '/iam/projects':
      return 'folder';
    case '/iam/users':
      return 'users';
    case '/iam/simulator':
      return 'play';
    case '/iam/identity-providers':
      return 'globe';
    case '/tools/ai-gateway-simulator':
      return 'bot';
    case '/setup':
      return 'wand';
    default:
      return 'file';
  }
}

export function NavIconForPath({ path }: { path: string }) {
  return <SidebarIconGlyph name={navIconNameForPath(path)} />;
}
