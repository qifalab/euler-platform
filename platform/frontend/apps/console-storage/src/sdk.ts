/**
 * Shared SDK instance for console-storage (02§9.1).
 * Wired to the auth link via @eu/wujie-bridge: getToken reads the base's LIVE
 * token (getter prop → auth:token-refreshed bus cache → legacy snapshot), and
 * onUnauthorized re-reads it after the base's silent refresh. Views must import
 * this instead of calling createSDK({}) with no auth options.
 */
import { createSDK } from "@eu/sdk";
import { subAppAuthOptions } from "@eu/wujie-bridge";

export const sdk = createSDK({ baseURL: "", ...subAppAuthOptions() });
