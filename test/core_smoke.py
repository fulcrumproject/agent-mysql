#!/usr/bin/env python3
# Exercise the real core and agent against an isolated, disposable PostgreSQL database.
# Only uniquely named or explicitly agent-labelled test resources are removed.
import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DOCKER = ["docker", "--host", "unix:///var/run/docker.sock"]
REDACT = []


def run(args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_until(check, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.3)
    raise RuntimeError("timed out waiting for expected behavior")


def safe(text):
    for value in REDACT:
        text = text.replace(value, "[REDACTED]")
    return text


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--core-repo", type=Path, default=ROOT.parent / "core")
    args = parser.parse_args()
    core_repo = args.core_repo.resolve()
    os.umask(0o077)
    tmp = Path(tempfile.mkdtemp(prefix="fulcrum-mysql-e2e-"))
    pg_name = tmp.name + "-postgres"
    api_port, health_port = free_port(), free_port()
    mysql_port = free_port()
    base = f"http://127.0.0.1:{api_port}/api/v1"
    processes, log_files = [], []
    agent_id = None
    occupied = None
    pg_started = False
    print(f"Private evidence directory: {tmp}", flush=True)

    def api(method, path, body=None, token="change-me", expected=None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(base + path, data=data, method=method,
                                         headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                status, payload = response.status, response.read()
        except urllib.error.HTTPError as error:
            status, payload = error.code, error.read()
        if expected is not None and status != expected:
            raise RuntimeError(f"{method} {path}: expected {expected}, received {status}: {safe(payload.decode())}")
        if expected is None and status >= 400:
            raise RuntimeError(f"{method} {path}: {status}: {safe(payload.decode())}")
        return json.loads(payload) if payload else None

    def items(path):
        return api("GET", path + "?pageSize=100")["items"]

    def service_jobs(sid):
        return [job for job in items("/jobs") if job["serviceId"] == sid]

    def job_done(sid, previous, status="Completed"):
        def check():
            new = [job for job in service_jobs(sid) if job["id"] not in previous]
            if not new:
                return False
            job = new[0]
            if job["status"] in ("Pending", "Processing"):
                return False
            if job["status"] != status:
                raise RuntimeError(f"Unexpected job result: {safe(json.dumps(job))}")
            return job
        return wait_until(check)

    def action(sid, name):
        previous = {job["id"] for job in service_jobs(sid)}
        api("POST", f"/services/{sid}/{name}")
        job_done(sid, previous)
        return api("GET", f"/services/{sid}")

    def launch(binary, env, log_name):
        stream = (tmp / log_name).open("w")
        log_files.append(stream)
        process = subprocess.Popen([str(binary)], cwd=tmp, env=env, stdout=stream, stderr=subprocess.STDOUT)
        processes.append(process)
        return process

    try:
        run(["go", "build", "-o", str(tmp / "core"), "./cmd/fulcrum"], cwd=core_repo)
        run(["go", "build", "-o", str(tmp / "agent"), "./cmd/agent"], cwd=ROOT)
        pg_password = secrets.token_hex(24)
        REDACT.append(pg_password)
        run(DOCKER + ["run", "-d", "--rm", "--name", pg_name, "--label", "io.fulcrum.mysql.test=" + tmp.name,
                      "-e", "POSTGRES_USER=fulcrum", "-e", "POSTGRES_DB=fulcrum", "-e", "POSTGRES_PASSWORD=" + pg_password,
                      "-p", "127.0.0.1::5432", "postgres:17-alpine"])
        pg_started = True
        pg_port = run(DOCKER + ["port", pg_name, "5432/tcp"]).rsplit(":", 1)[1]
        wait_until(lambda: subprocess.run(DOCKER + ["exec", pg_name, "pg_isready", "-U", "fulcrum"],
                                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0)
        dsn = f"host=127.0.0.1 port={pg_port} user=fulcrum password={pg_password} dbname=fulcrum sslmode=disable"
        env = {key: value for key, value in os.environ.items() if not key.startswith("FULCRUM_")}
        env.update({"FULCRUM_PORT": str(api_port), "FULCRUM_HEALTH_PORT": str(health_port),
                    "FULCRUM_PUBLIC_BASE_URL": f"http://127.0.0.1:{api_port}", "FULCRUM_AUTHENTICATORS": "token",
                    "FULCRUM_DB_DSN": dsn, "FULCRUM_METRIC_DB_DSN": dsn, "FULCRUM_SCHEDULER_LOCKER_DB_DSN": dsn,
                    "FULCRUM_VAULT_ENCRYPTION_KEY": secrets.token_hex(32), "FULCRUM_API_SERVER": "true",
                    "FULCRUM_JOB_MAINTENANCE": "false", "FULCRUM_AGENT_MAINTENANCE": "false", "FULCRUM_KEYCLOAK_ADMIN": "false"})
        core_process = launch(tmp / "core", env, "core.log")

        def ready():
            if core_process.poll() is not None:
                raise RuntimeError("core exited; inspect private core.log")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{health_port}/ready", timeout=1) as response:
                    return response.status == 200
            except OSError:
                return False
        wait_until(ready, 60)
        # The core's own seed creates this short-lived bootstrap token on an empty DB.
        provider = api("POST", "/participants", {"name": "mysql-test-provider", "status": "Enabled"})
        consumer = api("POST", "/participants", {"name": "mysql-test-consumer", "status": "Enabled"})
        load = lambda name: json.loads((ROOT / "examples" / name).read_text())
        service_type = api("POST", "/service-types", load("service-type.json"))
        agent_type_data = load("agent-type.json")
        agent_type_data["serviceTypeIds"] = [service_type["id"]]
        agent_type = api("POST", "/agent-types", agent_type_data)
        pool_set = api("POST", "/service-pool-sets", {"name": "mysql-test-ports", "providerId": provider["id"]})
        pool_data = load("port-pool.json")
        pool_data["servicePoolSetId"] = pool_set["id"]
        pool = api("POST", "/service-pools", pool_data)
        pool_value = api("POST", "/service-pool-values", {"name": str(mysql_port), "servicePoolId": pool["id"], "value": mysql_port})
        agent_record = api("POST", "/agents", {"name": "mysql-local-test", "providerId": provider["id"],
                         "agentTypeId": agent_type["id"], "servicePoolSetId": pool_set["id"],
                         "configuration": {"docker": {"bindIp": "127.0.0.1", "host": "127.0.0.1", "stateDir": str(tmp / "state")}}})
        agent_id = agent_record["id"]
        token = api("POST", "/tokens", {"name": "mysql-test-agent", "role": "agent", "scopeId": agent_id})["value"]
        REDACT.append(token)
        group = api("POST", "/service-groups", {"name": "mysql-test-group", "consumerId": consumer["id"]})
        for metric in load("metrics.json"):
            api("POST", "/metric-types", metric)
        env = {key: value for key, value in os.environ.items() if not key.startswith("MYSQL_AGENT_")}
        env.update({"MYSQL_AGENT_FULCRUM_API_URL": f"http://127.0.0.1:{api_port}", "MYSQL_AGENT_FULCRUM_API_TOKEN": token,
                    "MYSQL_AGENT_JOB_POLL_INTERVAL": "200ms", "MYSQL_AGENT_METRIC_REPORT_INTERVAL": "1s"})
        agent_process = launch(tmp / "agent", env, "agent.log")
        wait_until(lambda: api("GET", f"/agents/{agent_id}")["status"] == "Connected", 30)
        password = secrets.token_hex(24) + "'\\$"
        REDACT.append(password)
        occupied = socket.socket()
        occupied.bind(("127.0.0.1", mysql_port))
        occupied.listen()
        service_data = load("service.json")
        service_data.update({"agentId": agent_id, "serviceTypeId": service_type["id"], "groupId": group["id"]})
        service_data["properties"]["password"] = password
        service = api("POST", "/services", service_data)
        sid = service["id"]
        job_done(sid, set(), "Failed")
        assert api("GET", f"/services/{sid}")["status"] == "New"
        occupied.close()
        occupied = None
        print("PASS core failure path: port conflict preserved New service and resources", flush=True)
        service = action(sid, "create")
        assert service["status"] == "Started" and service["agentInstanceId"] == sid
        assert service["properties"]["password"].startswith("vault://")
        assert password not in json.dumps(service)
        assert service["agentInstanceData"]["port"] == mysql_port
        assert service["agentInstanceData"]["host"] == "127.0.0.1"
        ref = service["properties"]["password"].removeprefix("vault://")
        assert api("GET", f"/vault/secrets/{ref}", token=token)["value"] == password
        print("PASS core create retry: ready SQL, persistent vault secret and stable pooled endpoint", flush=True)
        api("PATCH", f"/services/{sid}", {"properties": {"cpu": 2}}, expected=400)
        api("DELETE", f"/services/{sid}", expected=400)
        service = action(sid, "stop")
        assert service["status"] == "Stopped"
        api("PATCH", f"/services/{sid}", {"properties": {"password": "rotation-is-out-of-scope"}}, expected=400)
        previous = {job["id"] for job in service_jobs(sid)}
        api("PATCH", f"/services/{sid}", {"properties": {"cpu": 2, "memory": 1536}})
        job_done(sid, previous)
        service = action(sid, "start")
        assert service["status"] == "Started" and service["properties"]["cpu"] == 2
        assert service["agentInstanceData"]["port"] == mysql_port
        wait_until(lambda: len(items("/metric-entries")) >= 5, 20)
        print("PASS core stop/update/start, immutable credentials, lifecycle guards and metric delivery", flush=True)
        action(sid, "stop")
        service = action(sid, "delete")
        assert service["status"] == "Deleted"
        assert not api("GET", f"/service-pool-values/{pool_value['id']}").get("serviceId")
        api("GET", f"/vault/secrets/{ref}", token=token, expected=404)
        assert not (tmp / "state" / agent_id / sid).exists()
        agent_process.terminate()
        agent_process.wait(timeout=40)
        assert agent_process.returncode == 0
        log = (tmp / "agent.log").read_text()
        assert password not in log
        print("PASS core delete: resources, vault secret and port allocation released; clean agent shutdown", flush=True)
        print("Core revision: " + run(["git", "rev-parse", "HEAD"], cwd=core_repo), flush=True)
        print("PASS real core smoke test", flush=True)
    finally:
        if occupied is not None:
            occupied.close()
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=40)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        for stream in log_files:
            stream.close()
        if agent_id:
            # Only resources carrying this fresh test agent identity are candidates.
            filters = ["--filter", "label=io.fulcrum.agent=" + agent_id, "--filter", "label=io.fulcrum.mysql=true"]
            for kind, listing, deletion in [("container", ["ps", "-aq"], ["rm", "-f"]),
                                             ("volume", ["volume", "ls", "-q"], ["volume", "rm"]),
                                             ("network", ["network", "ls", "-q"], ["network", "rm"])]:
                for resource in run(DOCKER + listing + filters).splitlines():
                    if kind == "container":
                        logs = subprocess.run(DOCKER + ["logs", "--tail", "100", resource], capture_output=True, text=True)
                        (tmp / (resource[:12] + ".log")).write_text(safe(logs.stdout + logs.stderr))
                    run(DOCKER + deletion + [resource])
        if pg_started:
            run(DOCKER + ["rm", "-f", pg_name])
        # Logs remain private for diagnosis; remove secret state and test binaries.
        import shutil
        shutil.rmtree(tmp / "state", ignore_errors=True)
        for binary in (tmp / "core", tmp / "agent"):
            binary.unlink(missing_ok=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        raise SystemExit(safe(str(error)))
