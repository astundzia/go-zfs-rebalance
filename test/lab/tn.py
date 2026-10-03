#!/usr/bin/env python3
"""Drive a TrueNAS SCALE 25.10 VM for the rebalance test lab (used by lab.sh; see README.md here).

  tn.py install   - hands-off install through the installer's JSON-RPC API (ws://127.0.0.1:18080/ws)
  tn.py setup     - after first boot: add the lab SSH key + passwordless sudo to truenas_admin, enable SSH
  tn.py call M P [--job] - call any middleware method (P is a JSON array of params); --job waits for the job
"""
import asyncio
import itertools
import json
import os
import sys
import time

LAB = os.environ.get("LAB_DIR") or os.path.expanduser("~/rebalance-lab")
sys.path.insert(0, os.path.join(LAB, "pylib"))  # lab.sh setup unpacks the websockets wheel here

import websockets  # noqa: E402

URL = "ws://127.0.0.1:18080"
PASSWORD = open(os.path.join(LAB, "truenas", "password")).read().strip()
ADMIN = "truenas_admin"


class RPC:
    def __init__(self, ws):
        self.ws = ws
        self.ids = itertools.count(1)

    async def call(self, method, *params, on_note=None):
        rid = next(self.ids)
        await self.ws.send(json.dumps({"jsonrpc": "2.0", "id": rid, "method": method, "params": list(params)}))
        while True:
            msg = json.loads(await self.ws.recv())
            if msg.get("id") == rid:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {json.dumps(msg['error'])}")
                return msg.get("result")
            if on_note and "method" in msg:
                on_note(msg)


async def connect(path, timeout=900):
    deadline = time.time() + timeout
    while True:
        try:
            return await websockets.connect(URL + path, max_size=None, open_timeout=10)
        except Exception as e:  # VM still booting
            if time.time() > deadline:
                raise SystemExit(f"could not reach {URL + path}: {e}")
            await asyncio.sleep(5)


async def install():
    async with await connect("/ws") as ws:
        rpc = RPC(ws)
        if not await rpc.call("is_adopted"):
            await rpc.call("adopt")
        disks = await rpc.call("list_disks")
        print("disks:", json.dumps(disks))
        boot = max(disks, key=lambda d: d["size"])["name"]  # 32G boot disk; data disks are 10G
        print("installing to", boot, flush=True)

        def progress(msg):
            if msg.get("method") == "installation_progress":
                p = msg["params"][0]
                print(f"  {p.get('progress', 0):.0%} {p.get('message', '')}", flush=True)

        await rpc.call("install", {
            "disks": [boot],
            "set_pmbr": False,
            "authentication": {"username": ADMIN, "password": PASSWORD},
            "post_install": {"network_interfaces": []},
        }, on_note=progress)
        print("install complete", flush=True)


async def middleware():
    ws = await connect("/api/current")
    rpc = RPC(ws)
    res = await rpc.call("auth.login_ex", {"mechanism": "PASSWORD_PLAIN", "username": ADMIN, "password": PASSWORD})
    if res.get("response_type") != "SUCCESS":
        raise SystemExit(f"login failed: {res}")
    return ws, rpc


async def wait_job(rpc, job_id, timeout=300):
    deadline = time.time() + timeout
    while time.time() < deadline:
        jobs = await rpc.call("core.get_jobs", [["id", "=", job_id]])
        if jobs and jobs[0]["state"] in ("SUCCESS", "FAILED", "ABORTED"):
            if jobs[0]["state"] != "SUCCESS":
                raise RuntimeError(f"job {job_id}: {jobs[0].get('error')}")
            return jobs[0].get("result")
        await asyncio.sleep(2)
    raise RuntimeError(f"job {job_id} timed out")


async def setup():
    ws, rpc = await middleware()
    async with ws:
        pubkey = open(os.path.join(LAB, "authorized_keys")).read().strip()
        user = (await rpc.call("user.query", [["username", "=", ADMIN]]))[0]
        await rpc.call("user.update", user["id"], {"sshpubkey": pubkey, "sudo_commands_nopasswd": ["ALL"]})
        await rpc.call("service.update", "ssh", {"enable": True})
        try:
            job = await rpc.call("service.control", "START", "ssh", {})
            await wait_job(rpc, job)
        except RuntimeError as e:
            if "service.control" not in str(e):
                raise
            await rpc.call("service.start", "ssh")
        print("ssh enabled for", ADMIN)


async def call(method, params, job):
    ws, rpc = await middleware()
    async with ws:
        res = await rpc.call(method, *params)
        if job:
            res = await wait_job(rpc, res, timeout=1800)
        print(json.dumps(res, indent=2, default=str))


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "install":
        asyncio.run(install())
    elif cmd == "setup":
        asyncio.run(setup())
    elif cmd == "call":
        args = [a for a in sys.argv[2:] if a != "--job"]
        asyncio.run(call(args[0], json.loads(args[1]) if len(args) > 1 else [], "--job" in sys.argv))
    else:
        print(__doc__)
        sys.exit(2)
