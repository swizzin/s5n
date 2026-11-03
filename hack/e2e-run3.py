#!/usr/bin/env python3
"""Run 3 for hack/e2e-kind.sh: edit objects through the app APIs from inside
the cluster (the same curl-in-pod style as the shell script's snapshot()),
then assert the wiring Job's narrow-ownership behavior on the next install —
a rename and a non-owned field survive, an owned field drifts and is
restored, a deleted owned object is recreated, and a user-added object is
left alone, each reported correctly in the receipt.

Usage:
  e2e-run3.py setup  <kube-context> <namespace> <work-dir>
  e2e-run3.py assert <kube-context> <namespace> <work-dir>

setup must run before the second `helm upgrade` of run 3; assert must run
after that upgrade, once <work-dir>/status-run3.json has been written from
the s5n-wiring ConfigMap.
"""
import base64
import json
import subprocess
import sys


def kubectl(ctx, ns, *args):
    return subprocess.run(
        ["kubectl", "--context", ctx, "-n", ns, *args],
        capture_output=True, text=True, check=True,
    ).stdout


def kubectl_stdin(ctx, ns, stdin_data, *args):
    return subprocess.run(
        ["kubectl", "--context", ctx, "-n", ns, *args],
        input=stdin_data, capture_output=True, text=True, check=True,
    ).stdout


def arr_port(ctx, ns, svc):
    return kubectl(ctx, ns, "get", "svc", svc, "-o", "jsonpath={.spec.ports[0].port}").strip()


def arr_key(ctx, ns, name):
    out = kubectl(ctx, ns, "get", "secret", "s5n-keys", "-o", f"jsonpath={{.data.{name}-apikey}}")
    return base64.b64decode(out).decode()


def curl_get(ctx, ns, pod, key, url):
    return json.loads(kubectl(ctx, ns, "exec", pod, "--", "curl", "-s", "-H", f"X-Api-Key: {key}", url))


def curl_put(ctx, ns, pod, key, url, body):
    kubectl_stdin(
        ctx, ns, json.dumps(body), "exec", "-i", pod, "--", "curl", "-s", "-X", "PUT",
        "-H", f"X-Api-Key: {key}", "-H", "Content-Type: application/json", "--data-binary", "@-", url,
    )


def curl_post(ctx, ns, pod, key, url, body):
    return json.loads(kubectl_stdin(
        ctx, ns, json.dumps(body), "exec", "-i", pod, "--", "curl", "-s", "-X", "POST",
        "-H", f"X-Api-Key: {key}", "-H", "Content-Type: application/json", "--data-binary", "@-", url,
    ))


def curl_delete(ctx, ns, pod, key, url):
    kubectl(ctx, ns, "exec", pod, "--", "curl", "-s", "-X", "DELETE", "-H", f"X-Api-Key: {key}", url)


def qbittorrent_password(ctx, ns):
    out = kubectl(ctx, ns, "get", "secret", "s5n-keys", "-o", "jsonpath={.data.qbittorrent-password}")
    return base64.b64decode(out).decode()


def expect_object(value, what):
    """Sonarr/Radarr/Prowlarr answer a failed save with a JSON array of
    validation failures instead of the object, even with forceSave=true, when
    the save still fails outright (e.g. a credential the live app rejects).
    Fail with that body rather than a confusing AttributeError two lines later."""
    if not isinstance(value, dict):
        sys.exit(f"setup: {what}: expected an object, got: {json.dumps(value)}")
    return value


def scrub_secrets(obj):
    """Secret fields are masked on every read after the write that set them,
    so a snapshot taken right after creation cannot be compared byte for byte
    against one read back later without first scrubbing them the same way."""
    out = json.loads(json.dumps(obj))
    for f in out.get("fields", []):
        if f.get("privacy") in ("password", "apiKey") or f.get("name") in ("password", "apiKey"):
            f["value"] = "SCRUBBED"
    return out


def setup(ctx, ns, work):
    sonarr_key, sonarr_port = arr_key(ctx, ns, "sonarr"), arr_port(ctx, ns, "sonarr")
    radarr_key, radarr_port = arr_key(ctx, ns, "radarr"), arr_port(ctx, ns, "radarr")
    prowlarr_key, prowlarr_port = arr_key(ctx, ns, "prowlarr"), arr_port(ctx, ns, "prowlarr")

    # sonarr: rename the owned client, change a non-owned field (priority) and
    # an owned one (tvCategory) in the same save.
    sonarr_url = f"http://localhost:{sonarr_port}/sonarr/api/v3/downloadclient"
    clients = curl_get(ctx, ns, "sonarr-0", sonarr_key, sonarr_url)
    if len(clients) != 1:
        sys.exit(f"setup: expected exactly one sonarr download client, found {len(clients)}")
    owned = clients[0]
    owned_id = owned["id"]
    owned["name"] = "qBit (renamed)"
    owned["priority"] = 7
    for f in owned["fields"]:
        if f["name"] == "tvCategory":
            f["value"] = "user-edit"
    curl_put(ctx, ns, "sonarr-0", sonarr_key, f"{sonarr_url}/{owned_id}?forceSave=true", owned)
    after_edit = curl_get(ctx, ns, "sonarr-0", sonarr_key, sonarr_url)
    edited = next((c for c in after_edit if c["id"] == owned_id), None)
    if edited is None or edited["name"] != "qBit (renamed)" or edited.get("priority") != 7:
        sys.exit(f"setup: the rename/priority/tvCategory edit did not take: {after_edit!r}")

    # sonarr: a second download client the Job never created and must never
    # touch. Its password must be the real qBittorrent credential, not a
    # placeholder: GET always masks the stored one (********), and a create
    # with a password the live qBittorrent actually rejects fails validation
    # outright even with forceSave=true.
    qbit_password = qbittorrent_password(ctx, ns)
    mine = json.loads(json.dumps(owned))
    mine.pop("id", None)
    mine["name"] = "mine"
    mine["priority"] = 1
    for f in mine["fields"]:
        if f["name"] == "tvCategory":
            f["value"] = "tv"
        if f["name"] == "password":
            f["value"] = qbit_password
    created = expect_object(
        curl_post(ctx, ns, "sonarr-0", sonarr_key, f"{sonarr_url}?forceSave=true", mine),
        "create the 'mine' download client",
    )
    with open(f"{work}/mine-before.json", "w", encoding="utf-8") as fh:
        json.dump(scrub_secrets(created), fh)

    # prowlarr: syncLevel is not owned.
    prowlarr_url = f"http://localhost:{prowlarr_port}/prowlarr/api/v1/applications"
    apps = curl_get(ctx, ns, "prowlarr-0", prowlarr_key, prowlarr_url)
    sonarr_app = next(a for a in apps if a["name"].lower().startswith("sonarr"))
    sonarr_app["syncLevel"] = "addOnly"
    curl_put(ctx, ns, "prowlarr-0", prowlarr_key, f"{prowlarr_url}/{sonarr_app['id']}?forceSave=true", sonarr_app)
    after_sync = curl_get(ctx, ns, "prowlarr-0", prowlarr_key, prowlarr_url)
    if not any(a["id"] == sonarr_app["id"] and a.get("syncLevel") == "addOnly" for a in after_sync):
        sys.exit(f"setup: the prowlarr syncLevel edit did not take: {after_sync!r}")

    # radarr: delete the owned client outright.
    radarr_url = f"http://localhost:{radarr_port}/radarr/api/v3/downloadclient"
    rclients = curl_get(ctx, ns, "radarr-0", radarr_key, radarr_url)
    if len(rclients) != 1:
        sys.exit(f"setup: expected exactly one radarr download client, found {len(rclients)}")
    radarr_old_id = rclients[0]["id"]
    curl_delete(ctx, ns, "radarr-0", radarr_key, f"{radarr_url}/{radarr_old_id}")
    if curl_get(ctx, ns, "radarr-0", radarr_key, radarr_url):
        sys.exit("setup: radarr download client was not actually deleted")

    with open(f"{work}/run3-setup.json", "w", encoding="utf-8") as fh:
        json.dump({"radarrOldID": radarr_old_id, "mineID": created["id"]}, fh)
    print(f"setup: edited sonarr client {owned_id}, added mine {created['id']}, "
          f"set prowlarr syncLevel addOnly, deleted radarr client {radarr_old_id}")


def check(ok, label):
    print(("ok  : " if ok else "FAIL: ") + label)
    return 0 if ok else 1


def do_assert(ctx, ns, work):
    failures = 0
    with open(f"{work}/run3-setup.json", encoding="utf-8") as fh:
        setup_info = json.load(fh)
    radarr_old_id, mine_id = setup_info["radarrOldID"], setup_info["mineID"]

    with open(f"{work}/status-run3.json", encoding="utf-8") as fh:
        status = json.load(fh)
    failures += check(status["status"] == "ok", f"wiring status ok (got {status['status']!r})")

    def step(name):
        return next((s for s in status["steps"] if s["name"] == name), None)

    sonarr_key, sonarr_port = arr_key(ctx, ns, "sonarr"), arr_port(ctx, ns, "sonarr")
    radarr_key, radarr_port = arr_key(ctx, ns, "radarr"), arr_port(ctx, ns, "radarr")
    prowlarr_key, prowlarr_port = arr_key(ctx, ns, "prowlarr"), arr_port(ctx, ns, "prowlarr")

    clients = curl_get(ctx, ns, "sonarr-0", sonarr_key, f"http://localhost:{sonarr_port}/sonarr/api/v3/downloadclient")
    others = [c for c in clients if c["id"] != mine_id]
    failures += check(len(others) == 1, f"exactly one sonarr client besides mine (got {len(others)})")
    if others:
        owned = others[0]
        failures += check(owned["name"] == "qBit (renamed)", f"sonarr client keeps its rename (got {owned['name']!r})")
        failures += check(owned.get("priority") == 7, f"sonarr client keeps priority 7 (got {owned.get('priority')!r})")
        tv_category = next((f["value"] for f in owned["fields"] if f["name"] == "tvCategory"), None)
        failures += check(tv_category == "tv", f"sonarr client's tvCategory restored to tv (got {tv_category!r})")

    mine_after = next((c for c in clients if c["id"] == mine_id), None)
    failures += check(mine_after is not None, "the user-added 'mine' client still exists")
    if mine_after is not None:
        with open(f"{work}/mine-before.json", encoding="utf-8") as fh:
            mine_before = json.load(fh)
        failures += check(scrub_secrets(mine_after) == mine_before, "'mine' is unchanged by the wiring Job")

    apps = curl_get(ctx, ns, "prowlarr-0", prowlarr_key, f"http://localhost:{prowlarr_port}/prowlarr/api/v1/applications")
    sonarr_app = next((a for a in apps if a["name"].lower().startswith("sonarr")), None)
    got_sync = sonarr_app.get("syncLevel") if sonarr_app else None
    failures += check(got_sync == "addOnly", f"prowlarr sonarr application keeps syncLevel addOnly (got {got_sync!r})")

    radarr_clients = curl_get(ctx, ns, "radarr-0", radarr_key, f"http://localhost:{radarr_port}/radarr/api/v3/downloadclient")
    failures += check(len(radarr_clients) == 1, f"radarr has an owned client again (got {len(radarr_clients)})")

    sonarr_step = step("sonarr/downloadclient") or {}
    drift = sonarr_step.get("drift") or []
    tv_drift = next((d for d in drift if d["field"] == "tvCategory"), None)
    failures += check(
        tv_drift is not None and tv_drift.get("old") == "user-edit" and tv_drift.get("new") == "tv",
        f"receipt shows sonarr/downloadclient drift tvCategory user-edit->tv (got {drift!r})",
    )

    radarr_step = step("radarr/downloadclient") or {}
    detail = radarr_step.get("detail", "")
    failures += check(
        "recreated" in detail and str(radarr_old_id) in detail,
        f"receipt shows radarr/downloadclient recreated mentioning id {radarr_old_id} (got {detail!r})",
    )

    return failures


if __name__ == "__main__":
    if len(sys.argv) != 5:
        sys.exit(__doc__)
    mode, ctx, ns, work = sys.argv[1:5]
    if mode == "setup":
        setup(ctx, ns, work)
        sys.exit(0)
    if mode == "assert":
        sys.exit(1 if do_assert(ctx, ns, work) else 0)
    sys.exit(f"unknown mode {mode!r}")
