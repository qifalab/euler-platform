--
-- eu-audit: audit side-stream plugin (07-security.md §6.1/§6.2, 03§4.4.3).
--
-- Position in the chain (07§5):
--   ip-restriction → limit-req → eu-auth → eu-authorize → [eu-audit] → upstream
--
-- Emits one audit event per OpenAPI call to Kafka cloud.sys.audit.action
-- (partitioned by account_id so a tenant's events stay ordered). A Go consumer
-- batch-writes them to ClickHouse, partitioned by day.
--
-- Retention (adjudication S24): ≥180 days hot in ClickHouse (等保三级 baseline)
-- plus MinIO cold backup; the sold product offers 365-day / 18-month tiers.
--
-- Two properties this plugin must preserve:
--
--   1. It never blocks the business request. Audit runs in log_by_lua (after
--      the response is sent) and failures are logged, not surfaced. 03§7 states
--      the audit topic must not block business processing.
--
--   2. It records the authorization decision, not just the call. A denied
--      request is exactly the one an auditor cares about, so events are emitted
--      for denials too — which is why this runs as a log phase handler rather
--      than being skipped when eu-authorize rejects.
--
-- Tamper-proofing (07§6.2): each event carries chain_hash, a per-tenant hash
-- chain computed downstream by svc-audit (the gateway does not hold the chain
-- state). Audit database accounts are separate from business accounts, and
-- developers have no edit/delete permission.
--
local core   = require("apisix.core")
local ngx    = ngx

local plugin_name = "eu-audit"

local schema = {
    type = "object",
    properties = {
        -- Kafka topic. Fixed by 04§5.4 — the single source of truth for topic
        -- names. Topic names carry no environment identifier; environments are
        -- isolated by cluster (C2/S8).
        topic = { type = "string", default = "cloud.sys.audit.action" },
        -- Kafka broker list, injected per environment.
        brokers = {
            type = "array",
            items = {
                type = "object",
                properties = {
                    host = { type = "string" },
                    port = { type = "integer" },
                },
                required = { "host", "port" },
            },
            minItems = 1,
        },
        -- Request parameters are recorded for the audit trail, but secrets must
        -- never land in the log. These parameter names are redacted.
        redact_params = {
            type = "array",
            items = { type = "string" },
            default = { "Password", "SecretKey", "SK", "Token", "SecurityToken", "PrivateKey" },
        },
        -- Bodies can be large; only record up to this many bytes.
        max_body_bytes = { type = "integer", minimum = 0, maximum = 65536, default = 4096 },
    },
    required = { "brokers" },
}

local _M = {
    version  = 0.1,
    priority = 2300,   -- after eu-authorize (2400)
    name     = plugin_name,
    schema   = schema,
}

function _M.check_schema(conf)
    return core.schema.check(schema, conf)
end

-- redact replaces the value of every sensitive parameter with a fixed marker.
-- Redaction happens here at the edge rather than downstream so a secret never
-- enters the audit pipeline at all.
local function redact(args, redact_list)
    if not args then
        return nil
    end
    local lookup = {}
    for _, name in ipairs(redact_list) do
        lookup[string.lower(name)] = true
    end
    local out = {}
    for k, v in pairs(args) do
        if lookup[string.lower(k)] then
            out[k] = "[REDACTED]"
        else
            out[k] = v
        end
    end
    return out
end

-- mask_ak shows only the last two characters of an access key, matching the
-- ak_id form in the audit event schema (07§6.1: "EU****3F").
local function mask_ak(ak)
    if not ak or #ak < 4 then
        return nil
    end
    return "EU****" .. string.sub(ak, -2)
end

function _M.log(conf, ctx)
    -- Build the audit event per the unified schema in 07§6.1.
    local account_id = ctx.eu_account_id
                       or core.request.header(ctx, "X-Euler-Account-Id")

    -- Unauthenticated requests that never reached eu-auth carry no identity;
    -- they are still recorded, since failed authentication is audit-relevant.
    local decision = "allow"
    local status = ngx.status
    if status == 403 or status == 401 then
        decision = "deny"
    end

    local event = {
        event_time   = ngx.time(),
        event_source = ctx.var.host,
        event_name   = ctx.eu_action or ngx.req.get_uri_args()["Action"],
        source_ip    = ctx.var.remote_addr,
        user_agent   = core.request.header(ctx, "User-Agent"),
        identity = {
            account_id  = account_id,
            principal   = ctx.eu_identity,
            ak_id       = mask_ak(ctx.eu_ak_id),
            -- MFA presence is carried on the identity when the console path
            -- established it; absent for signature auth.
            mfa_present = ctx.eu_mfa_present or false,
        },
        resource        = ctx.eu_resource_arn and { ctx.eu_resource_arn } or {},
        decision        = decision,
        decision_number = ctx.eu_decision_number,
        request_params  = redact(ngx.req.get_uri_args(), conf.redact_params),
        response_code   = status,
        -- trace_id links the audit record to the OTel trace and the ClickHouse
        -- logs — the "audit record → call chain → log" three-hop triage path
        -- described in 07§6.2.
        trace_id = core.request.header(ctx, "X-Euler-TraceId") or ngx.var.request_id,
    }

    local payload, err = core.json.encode(event)
    if not err and payload then
        -- Partition by account_id so a tenant's events preserve ordering
        -- (04§5.4). Emission is fire-and-forget: an audit pipeline problem
        -- must never surface as a business error.
        local ok, kafka_err = pcall(function()
            local producer = require("resty.kafka.producer")
            local bp = producer:new(conf.brokers, { producer_type = "async" })
            return bp:send(conf.topic, tostring(account_id or "anonymous"), payload)
        end)
        if not ok then
            core.log.error("eu-audit: kafka emit failed: ", kafka_err)
        end
    else
        core.log.error("eu-audit: encode failed: ", err)
    end
end

return _M
