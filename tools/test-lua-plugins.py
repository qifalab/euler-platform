"""Behavioural tests for the APISIX eu-auth / eu-authorize / eu-audit plugins,
run under a real Lua VM.

check-lua-plugins.py only checks structure. This harness loads eu-auth.lua,
eu-authorize.lua and eu-audit.lua with stub `apisix.core`, `resty.http`,
`resty.jwt`, `resty.kafka.producer` and `ngx` objects and drives
_M.rewrite() / _M.access() / _M.log() through the security-relevant paths:

  * client-supplied X-Euler-* / X-Eu-* headers are stripped — including when
    hidden behind 100+ padding headers (ngx.req.get_headers' default cap);
  * only RS256 access tokens pass (no HS256 alg confusion, no JWE "enc",
    no "crit", no MFA "typ" tokens);
  * the internal token is injected upstream only after authentication;
  * signature mode forwards the raw (still percent-encoded) path, the ngx
    arg/header shapes and the body as body_b64 to the :9100 verifier;
  * eu-authorize fails closed (400, no CheckAccess call) when a $arg_
    placeholder of resource_arn_template is missing, repeated, bare, empty
    or a wildcard, and talks to the :9101 check endpoint;
  * eu-audit reaches Kafka over TLS with certificate verification unless a
    route explicitly opts out, logs in with SCRAM (password from the env,
    never copied into the route conf), and skips the emit — rather than
    connecting unauthenticated — when the password is missing.

Requires `pip install lupa` (bundles a Lua runtime). Usage:
    python3 tools/test-lua-plugins.py
"""
import sys

try:
    from lupa import LuaRuntime
except ImportError:  # pragma: no cover
    print("SKIP: lupa not installed (pip install lupa)")
    sys.exit(0)

PLUGIN = "deploy/gitops-manifests/platform/apisix/plugins/eu-auth.lua"
AUTHORIZE_PLUGIN = "deploy/gitops-manifests/platform/apisix/plugins/eu-authorize.lua"
AUDIT_PLUGIN = "deploy/gitops-manifests/platform/apisix/plugins/eu-audit.lua"
VERIFY_URL = "http://svc-iam.euler-svc-iam.svc:9100/internal/openapi/verify"
CHECK_URL = "http://svc-iam.euler-svc-iam.svc:9101/internal/authz/check"

HARNESS = r"""
local INTERNAL = "s3cret-internal"
env = { EULER_INTERNAL_TOKEN = INTERNAL }
os.getenv = function(k) return env[k] end

-- request state ------------------------------------------------------------
local req_headers = {}
function reset_request(hdrs)
  req_headers = {}
  for k, v in pairs(hdrs) do req_headers[string.lower(k)] = v end
end

-- pure-Lua base64 (ngx.encode_base64 semantics: standard alphabet, padded)
local B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
local function b64(data)
  local out = {}
  for i = 1, #data, 3 do
    local a, b, c = string.byte(data, i, i + 2)
    local n = (a << 16) | ((b or 0) << 8) | (c or 0)
    local q = {}
    for s = 18, 0, -6 do
      local k = (n >> s) & 63
      q[#q + 1] = string.sub(B64, k + 1, k + 1)
    end
    if not b then q[3] = "="; q[4] = "=" elseif not c then q[4] = "=" end
    out[#out + 1] = table.concat(q)
  end
  return table.concat(out)
end

uri_args = {}
req_body = ""

ngx = {
  time = function() return 1000000 end,
  var = { request_id = "rid-1", request_uri = "/" },
  encode_base64 = b64,
  req = {
    -- like OpenResty: at most 100 headers unless max_headers == 0
    get_headers = function(max)
      local out, n = {}, 0
      local names = {}
      for k in pairs(req_headers) do table.insert(names, k) end
      table.sort(names, function(a, b)
        -- padding headers sort first so a smuggled header lands past 100
        local pa, pb = a:sub(1, 3) == "pad", b:sub(1, 3) == "pad"
        if pa ~= pb then return pa end
        return a < b
      end)
      for _, k in ipairs(names) do
        if max ~= 0 and n >= 100 then break end
        out[k] = req_headers[k]; n = n + 1
      end
      return out
    end,
    get_method = function() return "GET" end,
    read_body = function() end,
    get_body_data = function() return req_body end,
    get_body_file = function() return nil end,
    get_uri_args = function() return uri_args end,
  },
}

-- captured outbound calls ----------------------------------------------------
last_encoded = nil      -- last table passed to core.json.encode
json_decode_result = nil
http_enabled = false
http_calls = 0
http_response = nil
last_url, last_opts, last_payload = nil, nil, nil

package.preload["apisix.core"] = function()
  return {
    schema = { check = function() return true end },
    log = { error = function(...)
      local parts = {}
      for i = 1, select("#", ...) do parts[#parts + 1] = tostring((select(i, ...))) end
      last_log_error = table.concat(parts)
    end },
    json = {
      encode = function(v) last_encoded = v; return "{}" end,
      decode = function() return json_decode_result end,
    },
    lrucache = { new = function()
      return function(key, ver, create) return create() end
    end },
    request = {
      set_header = function(ctx, name, value) req_headers[string.lower(name)] = value end,
      header = function(ctx, name) return req_headers[string.lower(name)] end,
    },
  }
end
local httpc = {
  set_timeout = function() end,
  request_uri = function(self, url, opts)
    http_calls = http_calls + 1
    last_url, last_opts, last_payload = url, opts, last_encoded
    return http_response
  end,
}
package.preload["resty.http"] = function()
  return { new = function()
    if not http_enabled then return nil, "no http in tests" end
    return httpc
  end }
end

-- token format for the stub: "<alg>|<enc or ->|<crit or ->|<typ or ->|<account>"
whitelist_set = false
package.preload["resty.jwt"] = function()
  local jwt = {}
  local function split(t)
    local f = {}
    for p in string.gmatch(t, "[^|]+") do table.insert(f, p) end
    return f
  end
  function jwt:load_jwt(token)
    local f = split(token)
    if #f ~= 5 then return { valid = false } end
    local h = { alg = f[1] }
    if f[2] ~= "-" then h.enc = f[2] end
    if f[3] ~= "-" then h.crit = { f[3] } end
    return { valid = true, header = h }
  end
  function jwt:set_alg_whitelist(t) whitelist_set = (t.RS256 == 1) end
  function jwt:verify(key, token)
    -- an alg-confused verifier: accepts ANY alg (the plugin must pin it)
    local f = split(token)
    local p = { exp = 2000000, account_id = f[5] }
    if f[4] ~= "-" then p.typ = f[4] end
    return { verified = true, payload = p }
  end
  return jwt
end

-- lua-resty-kafka producer: records what eu-audit hands it.
kafka_new_calls = 0
kafka_brokers, kafka_conf, kafka_sent = nil, nil, nil
package.preload["resty.kafka.producer"] = function()
  local producer = {}
  function producer:new(broker_list, producer_config)
    kafka_new_calls = kafka_new_calls + 1
    kafka_brokers, kafka_conf = broker_list, producer_config
    return { send = function(self, topic, key, msg)
      kafka_sent = { topic = topic, key = key, msg = msg }
      return true
    end }
  end
  return producer
end

plugin = dofile(PLUGIN_PATH)
conf = { mode = "jwt", jwt_public_key = "PUBLIC KEY PEM" }

function run(hdrs)
  reset_request(hdrs)
  local ctx = {}
  local status = plugin.rewrite(conf, ctx)
  return status, req_headers
end

-- signature mode: one forward-auth round trip against a stub verifier
sig_conf = { mode = "signature", region = "cn-north-1", service = "ecs", timeout_ms = 500,
             verify_endpoint = plugin.schema.properties.verify_endpoint.default }
function run_sig(request_uri, args, hdrs, body)
  reset_request(hdrs)
  ngx.var.request_uri = request_uri
  uri_args = args
  req_body = body
  http_enabled = true
  http_calls = 0
  http_response = { status = 200, body = "",
                    headers = { ["X-Euler-Account-Id"] = "100123", ["X-Euler-Identity"] = "user/id:555" } }
  local ctx = { var = { host = "euecs.api.euler.emoera.com" } }
  local status = plugin.rewrite(sig_conf, ctx)
  http_enabled = false
  return status, last_payload, last_url, req_headers
end

authorize = dofile(AUTHORIZE_PATH)
authz_conf = { action = "euecs:DeleteInstance", region = "cn-north-1", timeout_ms = 500,
               resource_arn_template = "eu:ecs:$region:$account_id:instance/$arg_InstanceId",
               check_access_endpoint = authorize.schema.properties.check_access_endpoint.default }
function run_authz(args, allow)
  reset_request({})
  uri_args = args
  http_enabled = true
  http_calls = 0
  last_payload = nil
  http_response = { status = 200, body = "{}", headers = {} }
  json_decode_result = { allow = allow, decision_number = "D-1" }
  local ctx = { eu_account_id = "100123", eu_identity = "user/id:555",
                eu_internal_token = INTERNAL, var = { remote_addr = "10.0.0.9" } }
  local status = authorize.access(authz_conf, ctx)
  http_enabled = false
  return status, http_calls, last_payload, last_url, last_opts
end

-- eu-audit reads EULER_KAFKA_SASL_PASSWORD at load time, like eu-auth's token.
function load_audit(password)
  env.EULER_KAFKA_SASL_PASSWORD = password
  audit = dofile(AUDIT_PATH)
end
function run_audit(c)
  kafka_new_calls = 0
  kafka_brokers, kafka_conf, kafka_sent = nil, nil, nil
  last_log_error = nil
  reset_request({})
  uri_args = {}
  audit.log(c, { eu_account_id = "100123",
                 var = { host = "console.euler.emoera.com", remote_addr = "10.0.0.9" } })
end
"""

failures = 0


def check(name, cond):
    global failures
    print(("  OK    " if cond else "  FAIL  ") + name)
    if not cond:
        failures += 1


def main() -> int:
    lua = LuaRuntime(unpack_returned_tuples=True)
    lua.globals().PLUGIN_PATH = PLUGIN
    lua.globals().AUTHORIZE_PATH = AUTHORIZE_PLUGIN
    lua.globals().AUDIT_PATH = AUDIT_PLUGIN
    lua.execute(HARNESS)
    run = lua.globals().run

    good = "Bearer RS256|-|-|-|100123"
    spoof = {
        "Authorization": good,
        "X-Euler-Account-Id": "999",
        "X-Euler-Identity": "root",
        "X-Euler-Internal-Token": "guess",
        "X-Euler-Quota-Qps": "100000",
        "X-Eu-Uid": "999",
    }
    status, h = run(lua.table_from(spoof))
    check("valid RS256 token is accepted", status is None)
    check("account id comes from the token, not the client", h["x-euler-account-id"] == "100123")
    check("client X-Euler-Identity is stripped", h["x-euler-identity"] is None)
    check("client X-Euler-Quota-Qps is stripped", h["x-euler-quota-qps"] is None)
    check("client X-Eu-Uid is stripped", h["x-eu-uid"] is None)
    check("internal token is the gateway's, not the client's", h["x-euler-internal-token"] == "s3cret-internal")
    check("alg whitelist is applied", lua.globals().whitelist_set)

    padded = dict(spoof)
    for i in range(150):
        padded["pad-%03d" % i] = "x"
    status, h = run(lua.table_from(padded))
    check("identity header behind 150 padding headers is still stripped", h["x-euler-identity"] is None)

    for name, tok in {
        "HS256 (alg confusion)": "Bearer HS256|-|-|-|100123",
        "alg none": "Bearer none|-|-|-|100123",
        "JWE enc header": "Bearer RS256|A256GCM|-|-|100123",
        "crit header": "Bearer RS256|-|exp|-|100123",
        "MFA challenge token": "Bearer RS256|-|-|mfa|100123",
        "garbage": "Bearer garbage",
    }.items():
        status, h = run(lua.table_from({"Authorization": tok, "X-Euler-Account-Id": "999"}))
        check(name + " is rejected", status == 403)
        check(name + ": no internal token injected", h["x-euler-internal-token"] is None)
        check(name + ": spoofed account id stripped", h["x-euler-account-id"] is None)

    status, _ = run(lua.table_from({}))
    check("missing Authorization is 401", status == 401)

    # --- eu-auth anonymous mode (public marketing routes) --------------------
    lua.execute('conf = { mode = "anonymous" }')
    status, h = run(lua.table_from(spoof))
    check("anonymous mode lets the request through", status is None)
    check("anonymous mode injects no account id", h["x-euler-account-id"] is None)
    check("anonymous mode strips client identity", h["x-euler-identity"] is None)
    check("anonymous mode vouches with the gateway's internal token",
          h["x-euler-internal-token"] == "s3cret-internal")
    lua.execute('conf = { mode = "jwt", jwt_public_key = "PUBLIC KEY PEM" }')

    # --- eu-auth signature mode: what reaches the verifier ------------------
    run_sig = lua.globals().run_sig
    check("verify endpoint defaults to the :9100 verify container",
          lua.eval("plugin.schema.properties.verify_endpoint.default") == VERIFY_URL)
    status, payload, url, h = run_sig(
        "/buckets/my%20bucket/%E6%96%87?Tag=b&Tag=a&dryrun&Action=Put",
        lua.eval('{ Tag = { "b", "a" }, dryrun = true, Action = "Put" }'),
        lua.table_from({"host": "euecs.api.euler.emoera.com", "x-cps-date": "20260804T093000Z"}),
        b"\x00\xff\xfe{bin}")  # bytes: passed to Lua unchanged (not UTF-8 encoded)
    check("signed request is accepted by the stub verifier", status is None)
    check("verify call goes to the configured endpoint", url == VERIFY_URL)
    check("path is forwarded raw (still percent-encoded, no query)",
          payload is not None and payload["path"] == "/buckets/my%20bucket/%E6%96%87")
    check("repeated query key is forwarded as an array",
          payload is not None and payload["query"]["Tag"][1] == "b" and payload["query"]["Tag"][2] == "a")
    check("bare query key is forwarded as true", payload is not None and payload["query"]["dryrun"] is True)
    check("body travels as body_b64 (binary-safe)",
          payload is not None and payload["body_b64"] == "AP/+e2Jpbn0=")
    check("no plain-string body is sent", payload is not None and payload["body"] is None)
    check("route-bound region/service are sent",
          payload is not None and payload["region"] == "cn-north-1" and payload["service"] == "ecs")
    check("verified identity injected upstream", h["x-euler-identity"] == "user/id:555")
    status, payload, _, _ = run_sig("/", lua.eval("{}"), lua.table_from({"host": "h"}), "")
    check("empty body is body_b64 \"\"", status is None and payload["body_b64"] == "")

    # --- eu-authorize: resource ARN placeholders fail closed ----------------
    run_authz = lua.globals().run_authz
    check("check endpoint defaults to web-auth :9101",
          lua.eval("authorize.schema.properties.check_access_endpoint.default") == CHECK_URL)
    status, calls, payload, url, opts = run_authz(lua.eval('{ InstanceId = "i-1" }'), True)
    check("single InstanceId is authorized", status is None and calls == 1)
    check("check call goes to the configured endpoint", url == CHECK_URL)
    check("ARN carries the concrete instance",
          payload is not None and payload["resource"] == "eu:ecs:cn-north-1:100123:instance/i-1")
    check("internal token sent to CheckAccess",
          opts is not None and opts["headers"]["X-Euler-Internal-Token"] == "s3cret-internal")
    status, calls, _, _, _ = run_authz(lua.eval('{ InstanceId = "i-1" }'), False)
    check("CheckAccess deny is 403", status == 403 and calls == 1)
    for name, args in {
        "repeated InstanceId": '{ InstanceId = { "i-1", "i-2" } }',
        "bare InstanceId": "{ InstanceId = true }",
        "missing InstanceId": "{}",
        "empty InstanceId": '{ InstanceId = "" }',
        "wildcard InstanceId": '{ InstanceId = "i-*" }',
        "single-char wildcard InstanceId": '{ InstanceId = "i-?" }',
    }.items():
        status, calls, _, _, _ = run_authz(lua.eval(args), True)
        check(name + " fails closed with 400", status == 400)
        check(name + ": no CheckAccess call with a hole-punched ARN", calls == 0)

    # --- eu-audit: Kafka transport security (SASL_SSL client listener) -----
    load_audit, run_audit = lua.globals().load_audit, lua.globals().run_audit
    brokers3 = ('{ { host = "kafka-0.kafka-headless.mw-mq.svc", port = 9092 },'
                '  { host = "kafka-1.kafka-headless.mw-mq.svc", port = 9092 },'
                '  { host = "kafka-2.kafka-headless.mw-mq.svc", port = 9092 } }')

    def audit_conf(extra=""):
        # Schema defaults are not applied by the core stub: pass the ones
        # log() needs besides the transport options under test.
        return lua.eval('{ topic = "cloud.sys.audit.action", redact_params = { "Password" },'
                        ' brokers = %s%s }' % (brokers3, extra))

    load_audit("scram-pw\n")  # trailing newline, as from a file-created Secret
    props = "audit.schema.properties"
    check("eu-audit ssl defaults to on", lua.eval(props + ".ssl.default") is True)
    check("eu-audit ssl_verify defaults to on", lua.eval(props + ".ssl_verify.default") is True)
    check("eu-audit SASL mechanism defaults to SCRAM-SHA-512",
          lua.eval(props + ".sasl.properties.mechanism.default") == "SCRAM-SHA-512")
    mechs = list(lua.eval(props + ".sasl.properties.mechanism.enum").values())
    check("eu-audit accepts SCRAM mechanisms only (no PLAIN)",
          sorted(mechs) == ["SCRAM-SHA-256", "SCRAM-SHA-512"])
    check("eu-audit schema has no password field (it comes from the env)",
          lua.eval(props + ".sasl.properties.password") is None)

    conf = audit_conf()
    run_audit(conf)
    g = lua.globals()
    check("TLS is on when the route says nothing", g.kafka_conf is not None and g.kafka_conf.ssl is True)
    check("broker certificate is verified when the route says nothing", g.kafka_conf.ssl_verify is True)
    check("producer stays async (never blocks the request)", g.kafka_conf.producer_type == "async")
    check("no SASL login when the route configures none", g.kafka_brokers[1].sasl_config is None)

    conf = audit_conf(', sasl = { user = "apisix-audit" }')
    run_audit(conf)
    sc = g.kafka_brokers[1].sasl_config if g.kafka_brokers else None
    check("SCRAM login uses the configured user", sc is not None and sc.user == "apisix-audit")
    check("SCRAM mechanism falls back to SCRAM-SHA-512", sc is not None and sc.mechanism == "SCRAM-SHA-512")
    check("SCRAM password comes from the env, trailing newline dropped",
          sc is not None and sc.password == "scram-pw")
    check("every broker carries the SASL config",
          all(g.kafka_brokers[i].sasl_config is not None for i in (1, 2, 3)))
    check("broker hosts and ports pass through",
          g.kafka_brokers[3].host == "kafka-2.kafka-headless.mw-mq.svc" and g.kafka_brokers[3].port == 9092)
    check("password is never written into the route conf",
          conf.brokers[1].sasl_config is None and conf.sasl.password is None)
    check("event is emitted to the topic, keyed by account",
          g.kafka_sent is not None and g.kafka_sent.topic == "cloud.sys.audit.action"
          and g.kafka_sent.key == "100123")

    run_audit(audit_conf(', sasl = { user = "apisix-audit", mechanism = "SCRAM-SHA-256" }'))
    check("explicit SCRAM-SHA-256 is honoured", g.kafka_brokers[1].sasl_config.mechanism == "SCRAM-SHA-256")

    run_audit(audit_conf(", ssl_verify = false"))
    check("ssl_verify = false keeps TLS on", g.kafka_conf.ssl is True and g.kafka_conf.ssl_verify is False)
    run_audit(audit_conf(", ssl = false"))
    check("ssl = false (explicit opt-out) also drops verification",
          g.kafka_conf.ssl is False and g.kafka_conf.ssl_verify is False)

    for name, pw in {"unset": None, "empty": "", "newline only": "\n"}.items():
        load_audit(pw)
        run_audit(audit_conf(', sasl = { user = "apisix-audit" }'))
        check("SASL password %s: no producer created (fails closed)" % name, g.kafka_new_calls == 0)
        check("SASL password %s: the skip is logged" % name,
              g.last_log_error is not None and "EULER_KAFKA_SASL_PASSWORD" in g.last_log_error)
    run_audit(audit_conf())
    check("without SASL configured the missing password is irrelevant", g.kafka_new_calls == 1)

    print()
    print("%d failure(s)" % failures)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
