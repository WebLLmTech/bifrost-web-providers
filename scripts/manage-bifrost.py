#!/usr/bin/env python3
"""Explicitly invoked deployment helper; credentials remain in memory.

Mount the existing Bifrost config volume read-only at /diagnostic and a private
backup directory at /state. This tool never creates provider keys or grants.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import time
import urllib.error
import urllib.request
import urllib.parse

parser = argparse.ArgumentParser()
parser.add_argument("mode", choices=["inspect", "backup", "configure", "rollback", "verify", "test"])
parser.add_argument("--gateway", default="http://chat:8080")
parser.add_argument("--database", default="/diagnostic/config.db")
parser.add_argument("--state", default="/state")
parser.add_argument("--provider", choices=["deepseek-web","yuanbao-web"])
parser.add_argument("--pin-first-key", action="store_true")
parser.add_argument("--key-id")
args = parser.parse_args()
db = sqlite3.connect("file:" + args.database + "?mode=ro", uri=True)
db.row_factory = sqlite3.Row
row = db.execute("SELECT token FROM sessions WHERE encryption_status='plain_text' AND expires_at > datetime('now') ORDER BY created_at DESC LIMIT 1").fetchone()
if row is None:
    raise SystemExit("No usable admin session; sign in to the Bifrost dashboard.")
session = row["token"]
state_dir = Path(args.state)
state_file = state_dir / "providers-before.json"
names = ["deepseek-web", "yuanbao-web", "modelscope", "qwen"]

def api(method, path, body=None, headers=None, timeout=45):
    req = urllib.request.Request(args.gateway + path, method=method,
        headers={"Authorization": "Bearer " + session, "Content-Type": "application/json", **(headers or {})},
        data=json.dumps(body).encode() if body is not None else None)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            data = response.read()
            return json.loads(data) if data else None
    except urllib.error.HTTPError as error:
        raise RuntimeError(f"{method} {path}: HTTP {error.code}") from None

def payload(provider):
    fields = ["network_config", "concurrency_and_buffer_size", "proxy_config",
              "send_back_raw_request", "send_back_raw_response", "store_raw_request_response",
              "custom_provider_config", "openai_config", "prompt_cache"]
    return {k: copy.deepcopy(provider[k]) for k in fields if k in provider}

def keys_fingerprint(connection=db):
    rows = connection.execute("SELECT * FROM config_keys ORDER BY provider,key_id").fetchall()
    fields = ["provider", "key_id", "name", "value", "models_json", "blacklisted_models_json", "aliases_json", "weight", "enabled"]
    logical = [{k: (json.loads(row[k] or "null") if k.endswith("_json") else row[k]) for k in fields if k in row.keys()} for row in rows]
    return hashlib.sha256(json.dumps(logical, sort_keys=True).encode()).hexdigest()

def providers():
    return {p["name"]: p for p in api("GET", "/api/providers")["providers"]}

if args.mode == "inspect":
    known = providers()
    for name in names:
        if name not in known:
            print(json.dumps({"provider": name, "exists": False}))
            continue
        p = api("GET", "/api/providers/" + name)
        records = db.execute("SELECT * FROM config_keys WHERE provider=?", (name,)).fetchall()
        print(json.dumps({"provider": name, "base_url": p.get("network_config", {}).get("base_url"),
            "base_provider_type": (p.get("custom_provider_config") or {}).get("base_provider_type"),
            "keys": [{"name": r["name"], "models": json.loads(r["models_json"] or "[]"),
                "aliases": json.loads(r["aliases_json"] or "{}")} for r in records]}, ensure_ascii=False))
    items = api("GET", "/api/plugins")
    print(json.dumps({"plugins": [{"name": p["name"], "enabled": p["enabled"], "path": p.get("path")} for p in items["plugins"]]}))
elif args.mode == "backup":
    state_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(state_dir, 0o700)
    known = providers()
    if not all(n in known for n in names[:2]):
        raise SystemExit("Expected existing deepseek-web and yuanbao-web providers")
    saved = {n: payload(api("GET", "/api/providers/" + n)) for n in names if n in known}
    plugin = next((p for p in api("GET", "/api/plugins")["plugins"] if p["name"] == "web-providers"), None)
    fd = os.open(state_file, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump({"providers": saved, "plugin": plugin, "keys_fingerprint": keys_fingerprint()}, f, indent=2)
    backup_path = state_dir / "config-before.db"
    fd = os.open(backup_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)
    backup = sqlite3.connect(backup_path)
    db.backup(backup)
    backup.close()
    print("Private SQLite and provider configuration backups saved.")
elif args.mode == "configure":
    saved = json.loads(state_file.read_text())
    plugin_config = {
        "listen_addr": "127.0.0.1:8792", "request_timeout_seconds": 600, "max_request_mb": 100,
        "providers": {
            "deepseek-web": {"type": "deepseek", "options": {}},
            "yuanbao-web": {"type": "yuanbao", "options": {"browserless_ws": "ws://browserless-chromium:3000"}},
            "modelscope": {"type": "modelscope", "options": {"base_url": "https://api-inference.modelscope.ai", "timeout_seconds": 540, "max_result_mb": 64}}
        }}
    plugin_body = {"enabled": True, "path": "/plugins/web-providers.so", "config": plugin_config}
    if saved["plugin"] is None:
        api("POST", "/api/plugins", {"name": "web-providers", **plugin_body})
    else:
        api("PUT", "/api/plugins/web-providers", plugin_body)
    for name, original in saved["providers"].items():
        target = payload(api("GET", "/api/providers/" + name))
        instance = "modelscope" if name in ["modelscope", "qwen"] else name
        network = target.setdefault("network_config", {})
        network.update(base_url="http://127.0.0.1:8792/providers/" + instance,
                       allow_private_network=True, max_retries=0)
        if name in ["modelscope", "qwen"]:
            network["default_request_timeout_in_seconds"] = 600
        custom = target.setdefault("custom_provider_config", {})
        custom["base_provider_type"] = "openai"
        allowed = custom.setdefault("allowed_requests", {})
        allowed.update(list_models=True, chat_completion=True, chat_completion_stream=True,
                       responses=True, responses_stream=True)
        if name in ["modelscope", "qwen"]:
            allowed.update(image_generation=True, image_edit=True, image_generation_stream=False, image_edit_stream=False)
        api("PUT", "/api/providers/" + name, target)
        print("Configured existing provider", name, "->", network["base_url"])
    original_db=sqlite3.connect("file:"+str(state_dir/"config-before.db")+"?mode=ro&immutable=1",uri=True)
    original_db.row_factory=sqlite3.Row
    if keys_fingerprint() != keys_fingerprint(original_db):
        raise RuntimeError("Provider key metadata changed; investigate before continuing")
    print("Provider keys, aliases, model restrictions and weights unchanged.")
elif args.mode == "verify":
    original_db=sqlite3.connect("file:"+str(state_dir/"config-before.db")+"?mode=ro&immutable=1",uri=True)
    original_db.row_factory=sqlite3.Row
    if keys_fingerprint()!=keys_fingerprint(original_db):
        raise RuntimeError("Provider key metadata changed")
    print("Verified all existing keys, aliases, restrictions and weights unchanged (semantic JSON comparison).")
    for name in names:
        p=api("GET","/api/providers/"+name)
        expected="http://127.0.0.1:8792/providers/"+("modelscope" if name in ["modelscope","qwen"] else name)
        if p.get("network_config",{}).get("base_url")!=expected:
            raise RuntimeError("Unexpected provider endpoint for "+name)
        print(name+" -> "+expected)
elif args.mode == "rollback":
    saved = json.loads(state_file.read_text())
    for name, original in saved["providers"].items():
        api("PUT", "/api/providers/" + name, original)
    if saved["plugin"] is None:
        api("DELETE", "/api/plugins/web-providers")
    else:
        before = saved["plugin"]
        api("PUT", "/api/plugins/web-providers", {k: before[k] for k in ["enabled", "path", "config", "placement", "order"] if k in before})
    print("Restored provider routes and plugin configuration.")
elif args.mode == "test":
    # Admin session authenticates management. Inference uses an existing active
    # virtual key loaded only in memory; no provider token is exported.
    tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    candidates = [n for n in tables if "virtual" in n and "key" in n]
    result = api("GET", "/api/governance/virtual-keys?limit=100")
    active = [v for v in result["virtual_keys"] if v.get("is_active")]
    selected = None
    for v in active:
        granted = {p["provider"] for p in v.get("provider_configs", [])}
        if v.get("allow_all_providers") or all(n in granted for n in names[:2]):
            selected = v
            break
    if selected is None:
        raise SystemExit("No active existing virtual key grants both target providers")
    token = None
    for table in candidates:
        columns = {r[1] for r in db.execute('PRAGMA table_info("' + table + '")')}
        if not {"id", "value"}.issubset(columns):
            continue
        record = db.execute('SELECT * FROM "' + table + '" WHERE id=?', (selected["id"],)).fetchone()
        if record:
            token = record["value"]
            try:
                decoded = json.loads(token)
                if isinstance(decoded, dict):
                    token = decoded.get("value", "")
            except (ValueError, TypeError):
                pass
            break
    if not token:
        raise SystemExit("Could not resolve the existing virtual key without exporting credentials")
    failures=0
    for name in ([args.provider] if args.provider else names[:2]):
        records = db.execute("SELECT key_id,aliases_json FROM config_keys WHERE provider=? AND enabled=1", (name,)).fetchall()
        aliases = {}
        for record in records:
            aliases.update(json.loads(record["aliases_json"] or "{}"))
        model = next((m for m in ["chat-base", "base", "pro", "base_think"] if m in aliases), next(iter(aliases), "deepseek" if name == "deepseek-web" else "hunyuan_gpt_175B_0404"))
        for stream in [False, True]:
            body = {"model": name + "/" + model, "messages": [{"role": "user", "content": "只回复 OK"}], "stream": stream}
            req = urllib.request.Request(args.gateway + "/v1/chat/completions", method="POST",
                headers={"x-bf-vk": token, "Content-Type": "application/json", **({"x-bf-api-key-id": next((r["key_id"] for r in records if json.loads(r["aliases_json"] or "{}")), records[0]["key_id"])} if args.pin_first_key else {}), **({"x-bf-api-key-id":args.key_id} if args.key_id else {})},
                data=json.dumps(body).encode())
            started = time.monotonic()
            try:
                with urllib.request.urlopen(req, timeout=180) as response:
                    raw = response.read().decode()
                    chunks = [json.loads(line[6:]) for line in raw.splitlines() if line.startswith("data: ") and line != "data: [DONE]"] if stream else [json.loads(raw)]
                    errors = [c.get("error") for c in chunks if c.get("error")]
                    text = "".join(choice.get("delta" if stream else "message", {}).get("content") or "" for c in chunks for choice in c.get("choices", []))
                    finish = [choice.get("finish_reason") for c in chunks for choice in c.get("choices", []) if choice.get("finish_reason")]
                    if errors or not finish or not text.strip():failures+=1
                    print(json.dumps({"provider": name, "model_alias": model, "stream": stream, "status": response.status,
                        "seconds": round(time.monotonic() - started, 2), "content": text[:100], "finished": bool(finish),
                        "errors": errors}, ensure_ascii=False))
            except urllib.error.HTTPError as error:
                failures+=1
                body = error.read().decode()
                # Scrub all provider tokens and the virtual/admin key before printing a bounded diagnostic.
                secrets = [token, session]
                for record in db.execute("SELECT value FROM config_keys"):
                    value = record[0]
                    secrets.append(value)
                    try:
                        parsed = json.loads(value)
                        if isinstance(parsed, dict):
                            secrets.append(parsed.get("value", ""))
                    except (ValueError, TypeError):
                        pass
                for secret in secrets:
                    if secret:
                        body = body.replace(secret, "[redacted]")
                print(json.dumps({"provider": name, "model_alias": model, "stream": stream, "status": error.code, "error": body[:1200]}, ensure_ascii=False))

    if failures:
        raise SystemExit(f"{failures} live calls failed")
