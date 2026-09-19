"""Re-measure which reasoning tiers the Qoder CN CLI actually applies per model.

The proxy clamps a request onto `reasoningLadders` in internal/qodercli, but that
table is a snapshot of the gateway's per-model `efforts` list, which can change
upstream. The CLI's own stdout shows nothing about the decision, so the only
evidence is the session transcript the CLI writes for each spawned worker.

Usage (from a shell, with the proxy running on 127.0.0.1:8095):

    python scripts/tier_matrix.py                      # all account models, 6 tiers
    python scripts/tier_matrix.py Qwen3.8-Flash        # one model
    python scripts/tier_matrix.py Qwen3.8-Flash none low xhigh

Budget warning: every cell is one real chat request. A full sweep of 14 models x 6
tiers is ~84 requests and can trip the account's short-window chat limit, which
surfaces as CLI error code 110 and an HTTP 500 from the proxy. Prefer measuring
one model at a time.
"""
import glob
import json
import os
import re
import sys
import time
import urllib.request

PROXY = os.environ.get("LINGMA_PROXY_URL", "http://127.0.0.1:8095")
TIERS = ["none", "low", "medium", "high", "xhigh", "max"]

# The CLI's per-run logs and transcripts live under its own config root.
QODER_HOME = os.path.expanduser(os.environ.get("QODER_HOME_DIR", "~/.qoder-cn"))
RUNS = os.path.join(QODER_HOME, "logs", "runs")
PROJECTS = os.path.join(QODER_HOME, "projects")


def find_transcript(sid):
    """Locate the CLI's session transcript by id rather than guessing the slug rule.

    The project directory name is a mangled absolute path (note the double dash after
    the drive letter), and the mangling for spaces is not documented. Searching by the
    uuid filename sidesteps guessing it.
    """
    hits = glob.glob(os.path.join(PROJECTS, "*", sid + ".jsonl"))
    return hits[0] if hits else None


def run_dirs():
    out = set()
    for d in glob.glob(os.path.join(RUNS, "*")):
        if os.path.exists(os.path.join(d, "manifest.json")):
            out.add(d)
    return out


def argv_of(d):
    try:
        with open(os.path.join(d, "manifest.json"), encoding="utf-8") as fh:
            return json.load(fh).get("argv", [])
    except Exception:
        return []


def flag_of(argv, name):
    if name in argv:
        i = argv.index(name)
        if i + 1 < len(argv):
            return argv[i + 1]
    return None


def session_id_of(d):
    log = os.path.join(d, "qodercli.log")
    if not os.path.exists(log):
        return None
    with open(log, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = re.search(r"session=([0-9a-f-]{36})", line)
            if m:
                return m.group(1)
    return None


def applied_tier(sid, wait_s=60):
    """Return (tier the CLI kept, thinking block chars) from the session transcript.

    reasoning_effort=none in the run log means either "explicitly off" or "tier was
    dropped", so the transcript's runtime-config entry plus the presence of a
    thinking block is what actually distinguishes them.
    """
    deadline = time.time() + wait_s
    while time.time() < deadline:
        path = find_transcript(sid)
        if path:
            tier, thinking, saw_assistant = None, 0, False
            with open(path, encoding="utf-8", errors="replace") as fh:
                for line in fh:
                    try:
                        row = json.loads(line)
                    except Exception:
                        continue
                    if row.get("type") == "runtime-config":
                        tier = row.get("reasoningEffort")
                    if row.get("type") == "assistant":
                        saw_assistant = True
                        for block in (row.get("message") or {}).get("content") or []:
                            if block.get("type") == "thinking":
                                thinking = max(thinking, len(block.get("thinking") or ""))
            if saw_assistant:
                return tier, thinking
        time.sleep(1.5)
    return None, -1


def request(model, tier):
    body = {
        "model": model,
        "max_tokens": 200,
        "thinking": {"type": "adaptive"},
        "output_config": {"effort": tier},
        "messages": [{"role": "user", "content": "用一句话说明你为什么在这里"}],
    }
    req = urllib.request.Request(
        PROXY + "/v1/messages",
        data=json.dumps(body).encode("utf-8"),
        headers={"content-type": "application/json", "anthropic-version": "2023-06-01"},
    )
    before = run_dirs()
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            json.loads(resp.read().decode("utf-8"))
    except Exception as exc:
        return "REQUEST-ERROR " + str(exc)[:60], None, -1
    for _ in range(20):
        time.sleep(1)
        mine = [d for d in run_dirs() - before
                if flag_of(argv_of(d), "--model") == model
                and flag_of(argv_of(d), "--reasoning-effort") == tier]
        if len(mine) == 1:
            sid = session_id_of(mine[0])
            if not sid:
                continue
            tier_kept, thinking = applied_tier(sid)
            return "ok", tier_kept, thinking
        if len(mine) > 1:
            return "AMBIGUOUS (another agent is probing this account)", None, -1
    return "no matching worker run", None, -1


def account_models():
    with urllib.request.urlopen(PROXY + "/v1/models", timeout=30) as resp:
        return [m["id"] for m in json.loads(resp.read().decode("utf-8"))["data"]]


def main():
    models = sys.argv[1:] or account_models()
    tiers = TIERS
    print("model".ljust(20) + "".join(t.center(14) for t in tiers))
    for model in models:
        cells = []
        for tier in tiers:
            status, kept, thinking = request(model, tier)
            if status != "ok":
                cells.append(status[:12])
            else:
                mark = "OK" if kept == tier else "DROP"
                cells.append("%s/%s/%d" % (kept, mark, thinking))
            print("\r%-20s %s" % (model, " / ".join(cells)), end="", flush=True)
        print()
    print("\nLegend: kept-tier / OK = CLI kept what was asked, DROP = silently ignored "
          "/ thinking-block chars (0 means thinking really turned off).")


if __name__ == "__main__":
    main()
