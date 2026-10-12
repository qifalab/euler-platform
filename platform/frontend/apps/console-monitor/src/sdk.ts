/**
 * Shared SDK instance for console-monitor (02§9.1).
 * Wired to the auth link via @eu/wujie-bridge: getToken reads the base's LIVE
 * token (getter prop → auth:token-refreshed bus cache → legacy snapshot), and
 * onUnauthorized asks the base for a silent refresh. Views must import this
 * instead of calling createSDK({ baseURL: "" }) with no auth options — such a
 * client sends no Authorization header and every request 401s at the gateway.
 */
import { createSDK } from "@eu/sdk";
import { subAppAuthOptions } from "@eu/wujie-bridge";

export const sdk = createSDK({ baseURL: "", ...subAppAuthOptions() });
