/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Build-time account-center origin (prod default: https://account.euler.emoera.com). */
  readonly VITE_ACCOUNT_BASE?: string;
}
