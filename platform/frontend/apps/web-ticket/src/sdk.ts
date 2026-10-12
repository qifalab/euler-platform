/**
 * The app's single @eu/sdk client (02§9.1), auth-wired through the Wujie
 * bridge: inside the console it sends the shell's live access token and asks
 * the shell to refresh on 401; standalone it redeems the refresh cookie. A bare
 * createSDK({ baseURL: "" }) sends no Authorization header, so every
 * gateway-fronted call was rejected with 401.
 */
import { createSDK } from "@eu/sdk";
import { subAppAuthOptions } from "@eu/wujie-bridge";

export const sdk = createSDK({ baseURL: "", ...subAppAuthOptions() });
