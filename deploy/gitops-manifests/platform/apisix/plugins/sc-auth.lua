--
-- sc-auth: OpenAPI authentication plugin (07-security.md §5, 04§3.4).
--
-- Position in the chain (07§5):
--   ip-restriction → limit-req → [sc-auth] → sc-authorize → audit → upstream
--
-- APISIX ships no cloud-vendor signature plugin, so this plugin bypasses to
-- svc-iam's internal verification endpoint via forward-auth semantics. The
-- CPS1-HMAC-SHA256 contract itself lives in svc-iam (and pkg-go/cps1) — this
-- plugin deliberately does NOT reimplement the algorithm. A second Lua
-- implementation would be a fourth place for the contract to drift, which
-- 03§9.4 rule ⑤ forbids.
--
-- Two auth modes, one of which must succeed:
--   * OpenAPI zone ({productCode}.api.starcloud.cn): CPS1 signature
--   * Console zone (console.starcloud.cn): JWT issued by svc-iam
--
-- On success the plugin injects the identity headers upstream services trust
-- (07§3.3): X-Sc-Account-Id, X-Sc-Identity, X-Sc-TraceId, plus X-Sc-Ak-Id and
-- X-Sc-Quota-Qps for the per-AK rate limiter that runs after this plugin.
--
-- Upstream services accept these headers ONLY from gateway mTLS / internal
-- CIDR; anything else presenting them is rejected 403 (07§4.3). This plugin
-- therefore strips any client-supplied X-Sc-* header before injecting its own.
--
local core = require("apisix.core")
local http = require("resty.http")
local jwt  = require("resty.jwt")
local ngx  = ngx

local plugin_name = "sc-auth"

local schema = {
    type = "object",
    properties = {
        -- svc-iam internal verification endpoint (04§3.4).
        verify_endpoint = {
            type = "string",
            default = "http://svc-iam.business.svc/internal/openapi/verify",
        },
        -- Auth mode for this route: "signature" (OpenAPI zone) or "jwt"
        -- (console zone). Set per-route rather than sniffed from the request,
        -- so a caller cannot downgrade from signature auth to a weaker mode.
        mode = {
            type = "string",
            enum = { "signature", "jwt" },
            default = "signature",
        },
        -- region/service are resolved from the ROUTE, never from client input.
        -- They participate in the derived signing key, so binding them here is
        -- what prevents a signature scoped to one product being replayed
        -- against another (07§4.1).
        region  = { type = "string" },
        service = { type = "string" },
        -- Endpoint budget is P99 < 10ms (local sig compute + cached AK
        -- metadata); a generous ceiling still fails fast under trouble.
        timeout_ms = { type = "integer", minimum = 50, maximum = 2000, default = 500 },
        -- Nacos-published public key for JWT verification; rotation is by kid
        -- (04§3.4). Either inline (jwt_public_key) or, preferred in K8s, a
        -- file path to the mounted sc-jwt-public-key Secret
        -- (jwt_public_key_file) — route files must not rely on ${VAR}
        -- interpolation, which APISIX does not perform in route config.
        jwt_public_key = { type = "string" },
        jwt_public_key_file = { type = "string" },
    },
    required = { "mode" },
}

local _M = {
    version  = 0.1,
    priority = 2500,   -- after limit-req (2600), before sc-authorize (2400)
    name     = plugin_name,
    schema   = schema,
}

function _M.check_schema(conf)
    local ok, err = core.schema.check(schema, conf)
    if not ok then
        return false, err
    end
    if conf.mode == "signature" and (not conf.region or not conf.service) then
        return false, "signature mode requires region and service"
    end
    if conf.mode == "jwt" and not (conf.jwt_public_key or conf.jwt_public_key_file) then
        return false, "jwt mode requires jwt_public_key or jwt_public_key_file"
    end
    return true
end

-- jwt_public_key_cache memoises Secret-mounted public keys by path. The
-- kubelet updates the mounted file on Secret rotation; the cache is refreshed
-- lazily every 60s so a rotated key is picked up without a reload.
local jwt_public_key_cache = {}

local function get_jwt_public_key(conf)
    if conf.jwt_public_key then
        return conf.jwt_public_key
    end
    local path = conf.jwt_public_key_file
    local cached = jwt_public_key_cache[path]
    local now = ngx.time()
    if cached and (now - cached.at) < 60 then
        return cached.key
    end
    local f, err = io.open(path, "r")
    if not f then
        core.log.error("sc-auth: cannot read jwt_public_key_file ", path, ": ", err)
        return cached and cached.key or nil
    end
    local key = f:read("*a")
    f:close()
    jwt_public_key_cache[path] = { key = key, at = now }
    return key
end

-- strip_client_identity_headers removes any X-Sc-* header the client sent.
-- Without this, a caller could present X-Sc-Account-Id and impersonate another
-- tenant on any upstream that trusts the header (07§4.3 内部防伪).
local function strip_client_identity_headers(ctx)
    local headers = ngx.req.get_headers()
    for name, _ in pairs(headers) do
        if string.sub(string.lower(name), 1, 5) == "x-sc-" then
            core.request.set_header(ctx, name, nil)
        end
    end
end

-- deny renders the platform's unified error body (03§9.3) and stops the chain.
-- The body carries the code and RequestId only; it never reveals which
-- verification step failed, to avoid handing an attacker an oracle.
local function deny(ctx, status, code, message)
    local request_id = core.request.header(ctx, "X-Request-Id") or ngx.var.request_id
    return status, core.json.encode({
        RequestId = request_id,
        Code      = code,
        Message   = message,
    })
end

-- verify_signature bypasses to svc-iam. The full request context is forwarded
-- because the signature covers method, path, query, headers, and body hash.
local function verify_signature(conf, ctx)
    local httpc, err = http.new()
    if not httpc then
        core.log.error("sc-auth: failed to create http client: ", err)
        return deny(ctx, 503, "Common.ServiceUnavailable", "auth backend unavailable")
    end
    httpc:set_timeout(conf.timeout_ms)

    -- Read the body: it participates in the signature via
    -- x-cps-content-sha256, so the verifier needs the exact bytes. A body
    -- larger than nginx's client_body_buffer_size is spooled to a temp file and
    -- get_body_data() returns nil — treating that as an empty body would fail
    -- every large request with a signature mismatch, so read the file too.
    ngx.req.read_body()
    local body = ngx.req.get_body_data()
    if not body then
        local body_file = ngx.req.get_body_file()
        if body_file then
            local f = io.open(body_file, "rb")
            if f then
                body = f:read("*a")
                f:close()
            end
        end
    end
    if not body then
        core.log.error("sc-auth: cannot read request body for signature verification")
        return deny(ctx, 503, "Common.InternalError", "cannot read request body")
    end

    -- Sign the RAW request target, not nginx's normalized $uri: $uri is
    -- percent-decoded and path-normalized, while cps1 canonicalises the path
    -- the client actually signed (strict RFC 3986, encoding preserved). A path
    -- containing an encoded octet would otherwise never verify.
    local raw_target = ngx.var.request_uri or ctx.var.uri or "/"
    local raw_path = string.match(raw_target, "^[^?]*") or raw_target
    if raw_path == "" then
        raw_path = "/"
    end

    local payload = {
        method  = ngx.req.get_method(),
        host    = ctx.var.host,
        path    = raw_path,
        query   = ngx.req.get_uri_args(),
        headers = ngx.req.get_headers(),
        body    = body,
        -- Route-bound, never client-supplied.
        region  = conf.region,
        service = conf.service,
    }

    local res, req_err = httpc:request_uri(conf.verify_endpoint, {
        method  = "POST",
        body    = core.json.encode(payload),
        headers = { ["Content-Type"] = "application/json" },
    })

    if not res then
        -- Fail closed: an unreachable verifier means we cannot prove the
        -- caller's identity, so the request must not proceed.
        core.log.error("sc-auth: verify endpoint unreachable: ", req_err)
        return deny(ctx, 503, "Common.ServiceUnavailable", "auth backend unavailable")
    end

    if res.status ~= 200 then
        local code = "IAM.SignatureDoesNotMatch"
        local decoded = res.body and core.json.decode(res.body)
        if decoded and decoded.Code then
            code = decoded.Code
        end
        return deny(ctx, 403, code, "authentication failed")
    end

    -- Inject the verified identity for upstream services and for the
    -- sc-authorize plugin that runs next.
    local account_id = res.headers["X-Sc-Account-Id"]
    local ak_id      = res.headers["X-Sc-Ak-Id"]
    local identity   = res.headers["X-Sc-Identity"]

    if not account_id then
        core.log.error("sc-auth: verify returned 200 without X-Sc-Account-Id")
        return deny(ctx, 503, "Common.InternalError", "auth backend contract violation")
    end

    core.request.set_header(ctx, "X-Sc-Account-Id", account_id)
    core.request.set_header(ctx, "X-Sc-Ak-Id", ak_id)
    if identity then
        core.request.set_header(ctx, "X-Sc-Identity", identity)
    end
    core.request.set_header(ctx, "X-Sc-TraceId", ngx.var.request_id)

    -- Stash on ctx so sc-authorize and the audit side-stream can read them
    -- without re-parsing headers.
    ctx.sc_account_id = account_id
    ctx.sc_ak_id      = ak_id
    ctx.sc_identity   = identity

    -- Per-AK quota for the limit-count plugin keyed on X-Sc-Ak-Id (04§3.5).
    local quota = res.headers["X-Sc-Quota-Qps"]
    if quota then
        core.request.set_header(ctx, "X-Sc-Quota-Qps", quota)
    end
    return nil
end

-- verify_jwt handles the console zone. Tokens are RS256, 15-minute access
-- tokens issued by svc-iam (adjudication S13); the refresh token never reaches
-- the gateway — it is a HttpOnly cookie scoped to /api/auth.
local function verify_jwt(conf, ctx)
    local auth = core.request.header(ctx, "Authorization")
    if not auth then
        return deny(ctx, 401, "IAM.Unauthorized", "missing credentials")
    end
    local token = string.match(auth, "^Bearer%s+(.+)$")
    if not token then
        return deny(ctx, 401, "IAM.Unauthorized", "malformed authorization header")
    end

    local public_key = get_jwt_public_key(conf)
    if not public_key then
        return deny(ctx, 503, "Common.ServiceUnavailable", "jwt public key unavailable")
    end
    local verified = jwt:verify(public_key, token)
    if not verified or not verified.verified then
        return deny(ctx, 403, "IAM.InvalidToken", "token verification failed")
    end

    local payload = verified.payload or {}
    -- Expiry is enforced by resty.jwt, but check explicitly: a token with no
    -- exp claim at all would otherwise pass.
    if not payload.exp or payload.exp < ngx.time() then
        return deny(ctx, 403, "IAM.TokenExpired", "token expired")
    end

    local account_id = payload.account_id and tostring(payload.account_id)
    if not account_id then
        return deny(ctx, 403, "IAM.InvalidToken", "token missing account_id")
    end

    core.request.set_header(ctx, "X-Sc-Account-Id", account_id)
    if payload.principal then
        core.request.set_header(ctx, "X-Sc-Identity", payload.principal)
    end
    core.request.set_header(ctx, "X-Sc-TraceId", ngx.var.request_id)

    ctx.sc_account_id = account_id
    ctx.sc_identity   = payload.principal
    return nil
end

function _M.rewrite(conf, ctx)
    -- Always strip client-supplied identity headers first, whatever the mode.
    strip_client_identity_headers(ctx)

    if conf.mode == "signature" then
        return verify_signature(conf, ctx)
    end
    return verify_jwt(conf, ctx)
end

return _M
