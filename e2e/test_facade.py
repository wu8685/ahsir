"""CMA-facade e2e: drive ahsir's CMA HTTP surface with the native Anthropic SDK,
against the deterministic `echo` provider. Validates the wire shapes the SDK
expects plus a full turn round-trip through the real scheduler + agent."""
import time

import anthropic
import pytest


def _texts_of(ev):
    return "".join(b.text or "" for b in ev.content)


def _drive_turn(client, session_id, text, max_events=50, deadline_s=30):
    """Open the live event stream, send one user message, collect until idle.
    Returns (agent_texts, stop_reason_type).

    The live SSE stream is the primary path, but the deterministic `echo`
    provider can reply faster than a lazily-connected SSE client subscribes
    (a real LLM's latency hides this window). Since every event is persisted,
    we reconcile from the authoritative event log if the live tail missed the
    reply — keeping the assertion deterministic without masking a real bug."""
    texts, stop = [], None
    end = time.time() + deadline_s
    with client.beta.sessions.events.stream(session_id=session_id) as stream:
        client.beta.sessions.events.send(
            session_id=session_id,
            events=[{"type": "user.message", "content": [{"type": "text", "text": text}]}],
        )
        for n, ev in enumerate(stream):
            if ev.type == "agent.message":
                texts.append(_texts_of(ev))
            elif ev.type == "session.status_idle":
                stop = ev.stop_reason.type
                break
            elif ev.type == "session.status_terminated":
                break
            if n + 1 >= max_events or time.time() > end:
                break
    if not texts or stop is None:
        # Reconcile from the persisted log (events.list is the source of truth).
        log = list(client.beta.sessions.events.list(session_id=session_id))
        texts = [_texts_of(e) for e in log if e.type == "agent.message"]
        idle = [e for e in log if e.type == "session.status_idle"]
        if idle:
            stop = idle[-1].stop_reason.type
    return texts, stop


# ----- resource wire shapes -----

def test_agent_create(agent):
    assert agent.type == "agent"
    assert agent.id.startswith("agent_")
    assert agent.name == "e2e-echo"


def test_environment_create(environment):
    assert environment.type == "environment"
    assert environment.id.startswith("env_")


def test_session_create(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id, title="chat")
    assert s.type == "session"
    assert s.id.startswith("sesn_")
    assert s.status == "idle"
    assert s.agent.id == agent.id
    assert s.environment_id == environment.id


# ----- the real turn round-trip (facade → scheduler → echo agent) -----

def test_session_single_turn(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    texts, stop = _drive_turn(client, s.id, "hello agent")
    assert stop == "end_turn"
    assert texts, "expected at least one agent.message"
    assert "hello agent" in texts[-1]  # echo provider replies "echo: hello agent"


def test_session_multi_turn(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    t1, _ = _drive_turn(client, s.id, "first question")
    t2, _ = _drive_turn(client, s.id, "second question")
    assert "first question" in t1[-1]
    assert "second question" in t2[-1]

    events = list(client.beta.sessions.events.list(session_id=s.id))
    kinds = [e.type for e in events]
    assert kinds.count("agent.message") >= 2
    assert "session.status_idle" in kinds


# ----- session lifecycle -----

def test_session_lifecycle(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    assert s.id in {x.id for x in client.beta.sessions.list(agent_id=agent.id)}

    u = client.beta.sessions.update(s.id, title="renamed")
    assert u.title == "renamed"

    client.beta.sessions.archive(s.id)
    assert s.id not in {x.id for x in client.beta.sessions.list()}
    assert s.id in {x.id for x in client.beta.sessions.list(include_archived=True)}

    client.beta.sessions.delete(s.id)
    with pytest.raises(anthropic.NotFoundError):
        client.beta.sessions.retrieve(s.id)
