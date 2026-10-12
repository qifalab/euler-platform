/**
 * Test-only upstream services. The application still performs a real OIDC code
 * exchange, RS256/nonce/PKCE verification and real HTTP connector requests.
 * This process binds loopback only and is never included in production images.
 */
import http from "node:http";
import {
  randomBytes,
  generateKeyPairSync,
  createHash,
  sign,
} from "node:crypto";

const port = Number(process.env.FIXTURE_PORT || 19391);
const origin = `http://127.0.0.1:${port}`;
const publicOrigin =
  process.env.FIXTURE_PUBLIC_ORIGIN || "http://127.0.0.1:19390";
const clientID = "euler-browser-test";
const clientSecret = "test-only-client-secret";
const { privateKey, publicKey } = generateKeyPairSync("rsa", {
  modulusLength: 2048,
});
const jwk = {
  ...publicKey.export({ format: "jwk" }),
  kid: "fixture-key",
  alg: "RS256",
  use: "sig",
};
const flows = new Map(),
  codes = new Map(),
  tokens = new Map();
const users = {
  alice: {
    sub: "101",
    name: "Alice 测试用户",
    email: "alice@example.test",
    email_verified: true,
  },
  bob: {
    sub: "102",
    name: "Bob 测试用户",
    email: "bob@example.test",
    email_verified: true,
  },
};
const id = () => randomBytes(24).toString("base64url");
const json = (res, status, body) => {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Cache-Control": "no-store",
  });
  res.end(JSON.stringify(body));
};
const escape = (value) =>
  String(value).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
async function body(req) {
  let data = "";
  for await (const chunk of req) {
    data += chunk;
    if (data.length > 65536) throw new Error("body too large");
  }
  return data;
}
function jwt(claims) {
  const parts = [
    Buffer.from(JSON.stringify({ alg: "RS256", kid: jwk.kid })).toString(
      "base64url",
    ),
    Buffer.from(JSON.stringify(claims)).toString("base64url"),
  ];
  return (
    parts.join(".") +
    "." +
    sign("RSA-SHA256", Buffer.from(parts.join(".")), privateKey).toString(
      "base64url",
    )
  );
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, origin),
      path = url.pathname;
    if (path === "/healthz") return json(res, 200, { ready: true });
    if (path === "/readyz") return json(res, 200, { status: "ok" });
    if (path === "/.well-known/openid-configuration")
      return json(res, 200, {
        issuer: origin,
        authorization_endpoint: `${origin}/authorize`,
        token_endpoint: `${origin}/token`,
        userinfo_endpoint: `${origin}/userinfo`,
        jwks_uri: `${origin}/jwks`,
        response_types_supported: ["code"],
        subject_types_supported: ["public"],
        id_token_signing_alg_values_supported: ["RS256"],
        code_challenge_methods_supported: ["S256"],
        token_endpoint_auth_methods_supported: [
          "client_secret_basic",
          "client_secret_post",
        ],
      });
    if (path === "/jwks") return json(res, 200, { keys: [jwk] });
    if (path === "/authorize") {
      if (
        url.searchParams.get("client_id") !== clientID ||
        url.searchParams.get("redirect_uri") !==
          `${publicOrigin}/auth/callback` ||
        url.searchParams.get("response_type") !== "code" ||
        url.searchParams.get("code_challenge_method") !== "S256"
      )
        return json(res, 400, { error: "invalid authorization request" });
      const flowID = id();
      flows.set(flowID, Object.fromEntries(url.searchParams));
      res.writeHead(200, {
        "Content-Type": "text/html; charset=utf-8",
        "Cache-Control": "no-store",
      });
      return res.end(
        `<!doctype html><html lang="zh"><title>测试身份源</title><h1>选择测试账号</h1><p>此身份源仅用于自动化验收。</p><form method="post" action="/approve"><input type="hidden" name="flow" value="${escape(flowID)}"><button name="user" value="alice">Alice</button><button name="user" value="bob">Bob</button></form></html>`,
      );
    }
    if (path === "/approve" && req.method === "POST") {
      const form = new URLSearchParams(await body(req)),
        flow = flows.get(form.get("flow")),
        user = users[form.get("user")];
      if (!flow || !user)
        return json(res, 400, { error: "invalid test login" });
      flows.delete(form.get("flow"));
      const code = id();
      codes.set(code, { ...flow, user });
      const target = new URL(flow.redirect_uri);
      target.searchParams.set("code", code);
      target.searchParams.set("state", flow.state);
      res.writeHead(302, { Location: target.href });
      return res.end();
    }
    if (path === "/token" && req.method === "POST") {
      const form = new URLSearchParams(await body(req)),
        flow = codes.get(form.get("code"));
      const basic = Buffer.from(`${clientID}:${clientSecret}`).toString(
        "base64",
      );
      if (
        req.headers.authorization !== `Basic ${basic}` &&
        !(
          form.get("client_id") === clientID &&
          form.get("client_secret") === clientSecret
        )
      )
        return json(res, 401, { error: "invalid_client" });
      if (
        !flow ||
        form.get("redirect_uri") !== flow.redirect_uri ||
        createHash("sha256")
          .update(form.get("code_verifier") || "")
          .digest("base64url") !== flow.code_challenge
      )
        return json(res, 400, { error: "invalid_grant" });
      codes.delete(form.get("code"));
      const access = id(),
        now = Math.floor(Date.now() / 1000);
      tokens.set(access, flow.user);
      return json(res, 200, {
        token_type: "Bearer",
        access_token: access,
        expires_in: 3600,
        id_token: jwt({
          iss: origin,
          aud: clientID,
          iat: now,
          exp: now + 3600,
          nonce: flow.nonce,
          ...flow.user,
        }),
      });
    }
    if (path === "/userinfo") {
      const user = tokens.get(
        (req.headers.authorization || "").replace(/^Bearer /, ""),
      );
      return user
        ? json(res, 200, user)
        : json(res, 401, { error: "invalid_token" });
    }
    return json(res, 404, { error: "fixture route not found" });
  } catch {
    return json(res, 500, { error: "fixture request failed" });
  }
});
server.listen(port, "127.0.0.1", () =>
  console.log(`Test upstreams listening on ${origin}`),
);
process.on("SIGTERM", () => server.close());
process.on("SIGINT", () => server.close());
