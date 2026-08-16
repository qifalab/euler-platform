/**
 * @sc/tokens — StarCloud design tokens (02-frontend-architecture.md §10.1).
 *
 * Tokens are the single source of truth: this module is the TS shape, and the
 * accompanying `style.css` surfaces them as CSS custom properties (`--sc-*`).
 * `element.css` bridges Element Plus (`--el-*`) onto the same tokens.
 * The base shell sets these on :root; sub-apps consume them read-only and must
 * not redefine the `--sc-` prefix (02 §6.3 red line).
 *
 * Theme: "Google Cloud Glass" — Google Cloud palette + acrylic glass surfaces.
 */

import "./style.css";
import "./element.css";

export const tokens = {
  color: {
    brand: "#1a73e8",
    brandHover: "#1967d2",
    brandActive: "#185abc",
    brandSoft: "rgba(26, 115, 232, 0.08)",
    danger: "#d93025",
    success: "#188038",
    warning: "#f9ab00",
    warningText: "#b06000",
    textOnBrand: "#ffffff",
    bgPage: "#f8fafd",
    bgContainer: "#ffffff",
    textPrimary: "#202124",
    textSecondary: "#5f6368",
    textDisabled: "#bdc1c6",
    border: "#dadce0",
  },
  glass: {
    bg: "rgba(255, 255, 255, 0.62)",
    bgStrong: "rgba(255, 255, 255, 0.8)",
    bgSoft: "rgba(255, 255, 255, 0.5)",
    blur: "blur(20px) saturate(180%)",
    blurSoft: "blur(10px) saturate(160%)",
    border: "rgba(255, 255, 255, 0.6)",
    shadow: "0 1px 2px rgba(60,64,67,.08), 0 8px 24px rgba(60,64,67,.12)",
    overlay: "rgba(32, 33, 36, 0.55)",
    overlayLight: "rgba(32, 33, 36, 0.28)",
  },
  fontSize: {
    xs: "12px",
    sm: "13px",
    md: "14px",
    lg: "16px",
    xl: "20px",
  },
  fontFamily:
    '"Google Sans", "Roboto", "Segoe UI", "Helvetica Neue", "Noto Sans SC", "PingFang SC", "Microsoft YaHei", system-ui, sans-serif',
  spacing: {
    1: "4px",
    2: "8px",
    3: "12px",
    4: "16px",
    5: "20px",
    6: "24px",
    7: "32px",
    8: "40px",
  },
  radius: { sm: "6px", md: "8px", lg: "12px", xl: "16px" },
  shadow: {
    sm: "0 1px 2px rgba(60,64,67,.1)",
    md: "0 1px 3px rgba(60,64,67,.1), 0 4px 12px rgba(60,64,67,.12)",
    lg: "0 2px 6px rgba(60,64,67,.1), 0 12px 32px rgba(60,64,67,.16)",
  },
  transition: "180ms cubic-bezier(0.4, 0, 0.2, 1)",
  layout: {
    topbarHeight: "56px",
    siderWidth: "208px",
  },
  zIndex: {
    shell: "0-999",
    subApp: "1000-1999",
    globalModal: "2000",
  },
} as const;

export type Tokens = typeof tokens;
