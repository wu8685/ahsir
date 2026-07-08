"""Pytest fixtures that boot a real ahsir (scheduler + CMA facade) driven by the
deterministic `echo` provider, and hand back the official Anthropic SDK pointed
at the facade.

This is the CMA-facade end-to-end check for the cma-service→ahsir migration: the
native `anthropic` Python SDK talks CMA to `ahsir start --cma-listen`, which
drives THIS scheduler over loopback, which starts an echo-provider agent — the
full real path, no cma-service and no live LLM. `echo` replies `"echo: <prompt>"`
so assertions are stable.

Needs only `go`, `codesign`, and the `anthropic` Python SDK. Freshly-built
binaries are codesigned because this box SIGKILLs unsigned ones.
"""
import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path

import httpx
import pytest
from anthropic import Anthropic

REPO_ROOT = Path(__file__).resolve().parents[1]
ADMIN_TOKEN = "e2e-admin-token"


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def _go_env(**extra) -> dict:
    env = dict(os.environ)
    env["GO111MODULE"] = "on"
    # This box routes loopback through a proxy unless told otherwise.
    env["no_proxy"] = "127.0.0.1,localhost"
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env.update(extra)
    return env


def _wait_http(url: str, timeout: float = 90.0):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:  # noqa: S310
                if r.status < 500:
                    return
        except Exception as e:  # noqa: BLE001
            last = e
        time.sleep(0.4)
    raise RuntimeError(f"service at {url} not ready in {timeout}s (last={last})")


def _build_binaries(bindir: Path):
    """Build ahsir + ahsir-agent into one dir (the scheduler looks for
    ahsir-agent next to the ahsir executable) and codesign both so this box
    doesn't SIGKILL them."""
    for pkg, out in (("./cmd/ahsir", "ahsir"), ("./cmd/ahsir-agent", "ahsir-agent")):
        subprocess.run(
            ["go", "build", "-o", str(bindir / out), pkg],
            cwd=REPO_ROOT, env=_go_env(), check=True,
        )
    subprocess.run(
        ["codesign", "--force", "--sign", "-", str(bindir / "ahsir"), str(bindir / "ahsir-agent")],
        check=True,
    )


@pytest.fixture(scope="session")
def facade_url():
    """Boot `ahsir start --cma-listen` with the echo provider; yield the CMA
    facade base URL."""
    real = os.environ.get("CMA_E2E_FACADE_URL")
    if real:
        yield real
        return

    workdir = Path(tempfile.mkdtemp(prefix="ahsir-cma-e2e-"))
    bindir = workdir / "bin"
    bindir.mkdir()
    _build_binaries(bindir)

    sched_port = _free_port()
    facade_port = _free_port()
    range_start = _free_port()
    config = workdir / "ahsir.yaml"
    config.write_text(
        "agents: []\n"
        "registry:\n"
        '  host: "127.0.0.1"\n'
        f"  port: {sched_port}\n"
        "  heartbeat_interval: 10s\n"
        "  heartbeat_timeout: 30s\n"
        "timeouts:\n"
        "  chat: 10m\n"
        "  task_status: 30s\n"
        "port_range:\n"
        f"  start: {range_start}\n"
        f"  end: {range_start + 40}\n"
    )

    state = workdir / "cma-state.json"
    log = open(os.environ.get("CMA_E2E_LOG", str(workdir / "ahsir.log")), "w")
    proc = subprocess.Popen(
        [
            str(bindir / "ahsir"), "start",
            "--cma-listen", f"127.0.0.1:{facade_port}",
            "--cma-scheduler", f"http://127.0.0.1:{sched_port}",
            "--cma-state", str(state),
            str(config),
        ],
        cwd=REPO_ROOT,
        env=_go_env(
            CMA_RUNTIME_PROVIDER="echo",
            AHSIR_ADMIN_TOKEN=ADMIN_TOKEN,
            HOME=str(workdir),  # keep agent scaffolding under the temp dir
        ),
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    url = f"http://127.0.0.1:{facade_port}"
    try:
        _wait_http(f"{url}/v1/agents")
        yield url
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()
        shutil.rmtree(workdir, ignore_errors=True)


@pytest.fixture(scope="session")
def client(facade_url) -> Anthropic:
    """Official Anthropic SDK, pointed at the ahsir CMA facade. trust_env=False
    keeps the box's proxy off localhost calls."""
    return Anthropic(
        api_key="sk-cma-e2e",
        base_url=facade_url,
        http_client=httpx.Client(trust_env=False, timeout=60.0),
    )


# agent + environment are session-scoped: one echo agent, created once and
# reused across all tests. Per-test agent creation would churn agent processes
# (each agent = a spawned wrapper on its own port), and rapid create→start churn
# races the scheduler's port allocation. One long-lived agent mirrors real usage
# and keeps the suite deterministic; every test opens its own session on it.
@pytest.fixture(scope="session")
def agent(client):
    return client.beta.agents.create(
        name="e2e-echo",
        model="claude-opus-4-8",
        system="You are a concise assistant.",
    )


@pytest.fixture(scope="session")
def environment(client):
    return client.beta.environments.create(name="e2e-default", config={"type": "cloud"})
