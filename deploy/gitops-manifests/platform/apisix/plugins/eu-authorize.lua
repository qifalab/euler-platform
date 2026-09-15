--
-- eu-authorize: coarse-grained authorization plugin (07-security.md §3.4/§5).
--
-- Position in the chain (07§5):
--   ip-restriction → limit-req → eu-auth → [eu-authorize] → audit → upstream
--
-- Division of labour (07§3.4) — this boundary is the whole point of the plugin:
--
--   Gateway (here):  authentication (done by eu-auth) + interface-level
--                    authorization on the ACTION dimension + rate limiting.
--                    It does NOT query resource owners, because that would
--                    make the gateway depend on every product's database.
--
--   Service layer:   resource owner check (account_id must equal the resource
--                    owner) + business-semantic authorization, via the shared
--                    authz SDK (@RequireAuth annotation + owner-check aspect).
--
-- High-sensitivity interfaces (EUECS delete, EUOSS download, RAM changes) are
-- the exception: for those the gateway calls CheckAccess with the concrete ARN
-- so a fine-grained evaluation happens before the request reaches the service.
-- Configure them with resource_arn_template.
--
-- Caching (07§3.5):
--   L1  gateway node local memory, keyed by (api + resource + identity hash),
--       TTL 30s, passive expiry
--   L2  Redis, identity → policy set snapshot with policy_version
--   Invalidation: svc-iam bumps policy_version and publishes
--   cloud.sys.authz.policy.changed; convergence promise is ≤60s globally
--   (30s L1 expiry + event-based invalidation, double insurance — D5).
--
local core  = require("apisix.core")
local http  = require("resty.http")
local lrucache = core.lrucache.new({ ttl = 30, count = 10000 })  -- L1, 30s TTL
local ngx   = ngx

local plugin_name = "eu-authorize"

local schema = {
    type = "object",
    properties = {
        -- svc-iam CheckAccess endpoint.
        check_access_endpoint = {
            type = "string",
            default = "http://svc-iam.business.svc/internal/authz/check",
        },
        -- The action this route maps to, formatted {productCode}:{Operation}
        -- (07§3.1, S3). Bound to the route, never taken from the request.
        action = { type = "string" },
        -- Some routes multiplex several actions via the RPC-style Action
        -- parameter (04§3.2). When set, the action is read from this query
        -- parameter and prefixed with action_prefix.
        action_from_query = { type = "string" },
        action_prefix     = { type = "string" },
        -- Optional ARN template for fine-grained evaluation on high-risk
        -- interfaces. Supports $account_id, $region, and $arg_<name>
        -- placeholders, e.g.
        --   eu:ecs:$region:$account_id:instance/$arg_InstanceId
        resource_arn_template = { type = "string" },
        region     = { type = "string" },
        timeout_ms = { type = "integer", minimum = 50, maximum = 2000, default = 500 },
        -- Routes that only need authentication (e.g. DescribeRegions) can skip
        -- authorization entirely rather than paying a CheckAccess round trip.
        skip_authorization = { type = "boolean", default = false },
    },
}

local _M = {
    version  = 0.1,
    priority = 2400,   -- immediately after eu-auth (2500)
    name     = plugin_name,
    schema   = schema,
}

function _M.check_schema(conf)
    local ok, err = core.schema.check(schema, conf)
    if not ok then
        return false, err
    end
    if not conf.skip_authorization
       and not conf.action and not conf.action_from_query then
        return false, "either action or action_from_query is required"
    end
    if conf.action_from_query and not conf.action_prefix then
        return false, "action_from_query requires action_prefix"
    end
    return true
end

local function deny(ctx, status, code, message, decision)
    local body = {
        RequestId = core.request.header(ctx, "X-Request-Id") or ngx.var.request_id,
        Code      = code,
        Message   = message,
    }
    -- The decision number is an audit reference; the policy detail itself is
    -- never returned to the caller (07§3.4).
    if decision then
        body.DecisionNumber = decision
    end
    return status, core.json.encode(body)
end

-- resolve_action determines the action string for this request. Routes either
-- pin a single action or read it from the RPC-style Action query parameter,
-- which is then prefixed with the route's product code so a caller cannot
-- name an action belonging to another product.
local function resolve_action(conf)
    if conf.action then
        return conf.action
    end
    local args = ngx.req.get_uri_args()
    local raw = args[conf.action_from_query]
    if not raw or type(raw) ~= "string" then
        return nil
    end
    -- Reject anything that is not a bare operation name: an attacker must not
    -- be able to smuggle a ':' and forge a cross-product action.
    if not string.match(raw, "^[A-Za-z][A-Za-z0-9]*$") then
        return nil
    end
    return conf.action_prefix .. ":" .. raw
end

-- render_arn expands the ARN template for fine-grained evaluation.
local function render_arn(template, account_id, region)
    if not template then
        return nil
    end
    local args = ngx.req.get_uri_args()
    local arn = template
    arn = string.gsub(arn, "%$account_id", account_id or "")
    arn = string.gsub(arn, "%$region", region or "")
    arn = string.gsub(arn, "%$arg_([A-Za-z0-9_]+)", function(name)
        local v = args[name]
        if type(v) == "string" then
            return v
        end
        return ""
    end)
    return arn
end

-- call_check_access asks svc-iam to evaluate the policy set. The engine is
-- Deny-first with default-deny (07§3.3); the gateway never makes its own
-- allow/deny judgement.
local function call_check_access(conf, ctx, account_id, identity, action, resource_arn)
    local httpc, err = http.new()
    if not httpc then
        core.log.error("eu-authorize: http client: ", err)
        return nil, "backend"
    end
    httpc:set_timeout(conf.timeout_ms)

    local payload = {
        account_id = account_id,
        identity   = identity,
        action     = action,
        resource   = resource_arn,
        context    = {
            ["eu:SourceIp"]    = ctx.var.remote_addr,
            ["eu:CurrentTime"] = os.date("!%Y-%m-%dT%H:%M:%SZ"),
        },
    }

    local res, req_err = httpc:request_uri(conf.check_access_endpoint, {
        method  = "POST",
        body    = core.json.encode(payload),
        headers = { ["Content-Type"] = "application/json" },
    })
    if not res then
        core.log.error("eu-authorize: check endpoint unreachable: ", req_err)
        return nil, "backend"
    end
    if res.status ~= 200 then
        return nil, "backend"
    end

    local decoded = core.json.decode(res.body)
    if not decoded then
        return nil, "backend"
    end
    return decoded, nil
end

function _M.access(conf, ctx)
    if conf.skip_authorization then
        return nil
    end

    -- eu-auth must have run and established the identity. If it did not, the
    -- route is misconfigured; fail closed rather than treating the request as
    -- anonymous.
    local account_id = ctx.eu_account_id
                       or core.request.header(ctx, "X-Euler-Account-Id")
    if not account_id then
        core.log.error("eu-authorize: no identity on ctx; is eu-auth configured on this route?")
        return deny(ctx, 403, "IAM.NoPermission", "unauthenticated")
    end

    local action = resolve_action(conf)
    if not action then
        return deny(ctx, 400, "Common.InvalidParameter", "missing or malformed Action")
    end

    local identity = ctx.eu_identity or core.request.header(ctx, "X-Euler-Identity") or ""
    local resource_arn = render_arn(conf.resource_arn_template, account_id, conf.region)

    -- L1 cache: (identity, action, resource) → decision, 30s TTL.
    local cache_key = account_id .. "|" .. identity .. "|" .. action .. "|" .. (resource_arn or "*")

    local decision, err = lrucache(cache_key, nil, function()
        local d, e = call_check_access(conf, ctx, account_id, identity, action, resource_arn)
        if e then
            -- Do not cache backend failures: a transient outage must not pin a
            -- deny for 30 seconds.
            return nil, e
        end
        return d
    end)

    if err == "backend" or not decision then
        -- Fail closed: if the policy engine cannot be consulted we cannot
        -- prove the caller is permitted.
        return deny(ctx, 503, "Common.ServiceUnavailable", "authorization backend unavailable")
    end

    if not decision.allow then
        return deny(ctx, 403, "IAM.NoPermission", "no permission", decision.decision_number)
    end

    -- Record the action for the audit side-stream (07§6.1: every write leaves
    -- an audit trail, and the decision is part of the record).
    ctx.eu_action          = action
    ctx.eu_resource_arn    = resource_arn
    ctx.eu_decision_number = decision.decision_number
    return nil
end

return _M
