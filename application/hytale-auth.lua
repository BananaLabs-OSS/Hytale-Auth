local M = {}
local refresh_token, profile_uuid = "", ""
local setup_mode, device_code = false, ""
local poll_interval_seconds, last_poll_nanos = 5, 0

local function call(cell, provider, payload)
  return pulp.unpack(pulp.call_raw(cell, provider, pulp.pack(payload)))
end
local function http(request)
  request.timeout_ms = request.timeout_ms or 5000
  return call("http-json", "engine.http-json.v1.request", request)
end
local function kv_get(key) return call("scoped-kv-fs", "storage.kv-fs.v1.get", { key = key }) end
local function kv_put(key, value)
  return call("scoped-kv-fs", "storage.kv-fs.v1.put", { key = key, value = value })
end
local function oauth_refresh(token)
  local response = http({ method = "POST", url = pulp.config.token_url, form = {
    client_id = pulp.config.client_id, grant_type = "refresh_token", refresh_token = token,
  }})
  if response.status ~= 200 then error("oauth status " .. tostring(response.status)) end
  return response.value.access_token, response.value.refresh_token
end
local function start_device_flow()
  local response = http({ method = "POST", url = pulp.config.device_url, form = {
    client_id = pulp.config.client_id, scope = pulp.config.scope,
  }})
  if response.status ~= 200 then error("device auth status " .. tostring(response.status)) end
  local value = response.value
  device_code = value.device_code
  poll_interval_seconds = math.max(5, tonumber(value.interval) or 5)
  setup_mode = true
  print("Authorization required")
  print("   Go to: " .. tostring(value.verification_uri_complete))
  print("   Code:  " .. tostring(value.user_code))
end

function M.init(_)
  local token, profile = kv_get("refresh_token.txt"), kv_get("profile_uuid.txt")
  refresh_token = token.found and token.value or ""
  profile_uuid = profile.found and profile.value or ""
  if refresh_token ~= "" then
    local ok, _, rotated = pcall(oauth_refresh, refresh_token)
    if ok then
      if rotated ~= nil and rotated ~= "" and rotated ~= refresh_token then
        refresh_token = rotated; kv_put("refresh_token.txt", rotated)
      end
      return { ready = true }
    end
    refresh_token = ""
  end
  start_device_flow()
  return { ready = false }
end

function M.tick(payload)
  if not setup_mode then return { polled = false } end
  local now = tonumber(payload.wall_nanos) or 0
  if last_poll_nanos == 0 then last_poll_nanos = now; return { polled = false } end
  if now < last_poll_nanos or now - last_poll_nanos < poll_interval_seconds * 1000000000 then
    return { polled = false }
  end
  last_poll_nanos = now
  local response = http({ method = "POST", url = pulp.config.token_url, form = {
    client_id = pulp.config.client_id,
    grant_type = "urn:ietf:params:oauth:grant-type:device_code", device_code = device_code,
  }})
  local value = response.value or {}
  if value.error == "authorization_pending" then return { polled = true } end
  if value.error == "slow_down" then poll_interval_seconds = poll_interval_seconds + 5; return { polled = true } end
  if value.error == "expired_token" or value.error == "access_denied" then
    setup_mode = false; return { polled = true, terminal = true }
  end
  if value.error ~= nil and value.error ~= "" then return { polled = true } end
  if value.refresh_token == nil or value.refresh_token == "" then return { polled = true } end
  refresh_token = value.refresh_token; kv_put("refresh_token.txt", refresh_token)
  if value.access_token ~= nil and value.access_token ~= "" then
    local profiles = http({ method = "GET", url = pulp.config.profiles_url,
      headers = { Authorization = "Bearer " .. value.access_token } })
    if profiles.status == 200 and profiles.value.profiles ~= nil and profiles.value.profiles[1] ~= nil then
      profile_uuid = profiles.value.profiles[1].uuid; kv_put("profile_uuid.txt", profile_uuid)
    end
  end
  setup_mode = false
  print("Authorized!")
  return { polled = true, authorized = true }
end

function M.health(_) return { body = "ok", status = 200 } end
function M.status(_)
  if setup_mode then
    return { body = "Status: Needs Authorization\nSee cell logs for the verification URL and code.\n", status = 200 }
  end
  return { body = "Status: Ready\n", status = 200 }
end
function M.tokens(_)
  if setup_mode then return { status = 503, error = "Not authorized yet. Visit / for setup." } end
  local access, rotated = oauth_refresh(refresh_token)
  if rotated ~= nil and rotated ~= "" and rotated ~= refresh_token then
    refresh_token = rotated; kv_put("refresh_token.txt", rotated)
  end
  local response = http({ method = "POST", url = pulp.config.session_url,
    headers = { Authorization = "Bearer " .. access, ["Content-Type"] = "application/json" },
    body_text = '{"uuid": "' .. profile_uuid .. '"}' })
  if response.status ~= 200 then error("session status " .. tostring(response.status)) end
  return { status = 200, env = {
    HYTALE_SERVER_SESSION_TOKEN = response.value.sessionToken,
    HYTALE_SERVER_IDENTITY_TOKEN = response.value.identityToken,
  }}
end

local EVENTS = {
  ["hytale-auth.init.v1"] = M.init, ["hytale-auth.tick.v1"] = M.tick,
  ["hytale-auth.http.health.v1"] = M.health, ["hytale-auth.http.status.v1"] = M.status,
  ["hytale-auth.http.tokens.v1"] = M.tokens,
}
for event, handler in pairs(EVENTS) do pulp.on(event, handler) end
return M
