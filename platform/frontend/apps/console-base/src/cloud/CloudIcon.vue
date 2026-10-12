<script setup lang="ts">
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 20 });
const paths: Record<string, string> = {
  grid: "M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z",
  home: "m3 10 9-7 9 7 M5 9v12h14V9 M9 21v-8h6v8",
  layers: "m12 3 10 5-10 5L2 8z M2 12l10 5 10-5 M2 16l10 5 10-5",
  users:
    "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M16 3a4 4 0 0 1 0 8 M22 21v-2a4 4 0 0 0-3-3.87 M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0",
  shield: "M12 3 3 7v5c0 5 9 9 9 9s9-4 9-9V7z m-5 9 3 3 7-7",
  clock: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M12 7v5l3 2",
  arrow: "M5 12h14 m-5-5 5 5-5 5",
  external: "M14 3h7v7 M10 14 21 3 M21 14v7H3V3h7",
  plus: "M12 5v14 M5 12h14",
  close: "m6 6 12 12 M18 6 6 18",
  chevron: "m6 9 6 6 6-6",
  search: "M19 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0 m-2 5 5 6",
  refresh: "M20 7v5h-5 M4 17v-5h5 M6 6a8 8 0 0 1 13 2 M5 16a8 8 0 0 0 13 2",
  link: "m10 13 4-4 M8 16l-1 1a4 4 0 0 1-6-6l5-5a4 4 0 0 1 6 0 M16 8l1-1a4 4 0 0 1 6 6l-5 5a4 4 0 0 1-6 0",
  database:
    "M21 5c0 2-4 3-9 3S3 7 3 5s4-3 9-3 9 1 9 3 M3 5v14c0 2 4 3 9 3s9-1 9-3V5 M3 12c0 2 4 3 9 3s9-1 9-3",
  storage: "M3 5h18v6H3z M3 15h18v6H3z M7 8h.01 M7 18h.01",
  chart: "M4 3v18h17 M9 16v-5 M14 16V7 M19 16v-9",
  key: "M15 7a5 5 0 1 1-10 0 5 5 0 0 1 10 0 M10 12v9 M10 17h4 M10 21h3",
  globe:
    "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M3 12h18 M12 3c5 5 5 13 0 18-5-5-5-13 0-18",
  gift: "M3 8h18v4H3z M5 12v9h14v-9 M12 8v13 M12 8C2 8 5 0 9 3l3 5c10 0 7-8 3-5z",
  check: "m5 12 4 4L19 6",
  alert: "m12 3 10 18H2z M12 9v4 M12 17h.01",
  info: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M12 11v6 M12 7h.01",
  logout: "M9 3H3v18h6 M14 8l5 4-5 4 M8 12h11",
  menu: "M3 6h18 M3 12h18 M3 18h18",
  copy: "M8 8h13v13H8z M16 8V3H3v13h5",
  mail: "M3 5h18v14H3z m0 0 9 7 9-7",
  folder: "M3 5h7l2 3h9v13H3z",
  sun: "M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0 M12 2v2 M12 20v2 M2 12h2 M20 12h2 M5 5l1 1 M18 18l1 1 M5 19l1-1 M18 6l1-1",
};
</script>
<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.7"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path :d="paths[name] ?? paths.layers" />
  </svg>
</template>
