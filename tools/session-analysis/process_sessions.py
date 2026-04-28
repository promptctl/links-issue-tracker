#!/usr/bin/env python3
"""
Process Claude Code session JSONL files into compact structured digests
suitable for cross-session analysis (position tracing, cue placement,
affordance gap finding).

Input:  ~/.claude/projects/<encoded-project-dir>/<session-id>.jsonl
Output: per-session JSON + Markdown digest + index.json summary

Each session record is reduced from raw transcript (often MBs) to a
compact turn list with role, timestamps, thinking/text bodies, and
per-tool-call summaries. Tool results are intentionally dropped — the
agent's actions and reasoning are carried by tool_use + thinking + text;
results add bulk without changing the analysis signal much. Pair this
with the original .jsonl if a specific result needs inspecting.
"""

import argparse
import json
import os
import re
import sys
from collections import Counter
from pathlib import Path

DEFAULT_PROJECT = "-Users-bmf-code-links-issue-tracker"
DEFAULT_OUTDIR = "tools/session-analysis/processed"

# Truncation budgets — tuned empirically: real content rarely exceeds 2KB,
# so 3KB caps cut tail risk without dropping signal.
TRUNC_USER_TEXT = 3000
TRUNC_THINKING = 3000
TRUNC_ASSISTANT_TEXT = 3000
TRUNC_TOOL_ARGS = 300
TRUNC_BASH_CMD = 800
TRUNC_BASH_STDOUT = 300

# Wrapper patterns that are pure noise — strip them entirely.
NOISE_WRAPPERS = (
    "<local-command-caveat>",
    "<command-name>",
    "<command-message>",
    "<command-args>",
    "<local-command-stdout>",
)


def truncate(s, n):
    if not s:
        return s
    if len(s) <= n:
        return s
    return s[:n] + f"…[truncated {len(s) - n} chars]"


LIT_RE = re.compile(r"\blit\s+([a-z][a-z0-9-]*)")


def extract_lit_subcommands(bash_cmd):
    return LIT_RE.findall(bash_cmd or "")


BASH_TOOL_RE = re.compile(r"^\s*([a-zA-Z0-9_./-]+)")


def categorize_bash(cmd):
    """Coarse first-word bucket for histogramming."""
    m = BASH_TOOL_RE.match(cmd or "")
    if not m:
        return "?"
    head = m.group(1).split("/")[-1]
    return head


def summarize_tool_args(name, args):
    if not isinstance(args, dict):
        return truncate(str(args), TRUNC_TOOL_ARGS)
    if name == "Bash":
        return truncate(args.get("command", ""), TRUNC_BASH_CMD)
    if name in ("Edit", "Write", "Read", "NotebookEdit"):
        return args.get("file_path", "?")
    if name == "Grep":
        return f"pattern={args.get('pattern','')!r} path={args.get('path','') or '.'} mode={args.get('output_mode','files_with_matches')}"
    if name == "Glob":
        return f"pattern={args.get('pattern','')!r}"
    if name == "Skill":
        return f"{args.get('skill','?')} args={truncate(str(args.get('args','')), 200)!r}"
    if name == "Agent":
        return f"{args.get('subagent_type','general-purpose')}: {args.get('description','')}"
    if name == "TaskCreate" or name == "TaskUpdate":
        return f"{args.get('description','')} status={args.get('status','')}"
    if name == "WebFetch":
        return args.get("url", "?")
    if name == "ToolSearch":
        return args.get("query", "?")
    return truncate(json.dumps(args, default=str, sort_keys=True), TRUNC_TOOL_ARGS)


BASH_INPUT_RE = re.compile(r"<bash-input>([\s\S]*?)</bash-input>")
BASH_STDOUT_RE = re.compile(r"<bash-stdout>([\s\S]*?)</bash-stdout>")
BASH_STDERR_RE = re.compile(r"<bash-stderr>([\s\S]*?)</bash-stderr>")


def normalize_user_text(text):
    """Strip wrapper noise, compact terminal command I/O. Returns ('', kind) where kind indicates the turn shape."""
    if not text:
        return "", "empty"
    stripped = text.strip()
    # Pure-noise wrappers: drop entirely.
    if any(stripped.startswith(w) for w in NOISE_WRAPPERS):
        # If the only content is a noise wrapper, return empty so the turn is skipped.
        # But preserve any user text after the wrapper (some sessions append the real prompt after).
        # Conservative: keep text after the wrapper closes.
        for w in NOISE_WRAPPERS:
            if stripped.startswith(w):
                close = w.replace("<", "</")
                idx = stripped.find(close)
                if idx >= 0:
                    after = stripped[idx + len(close) :].strip()
                    if not after:
                        return "", "noise"
                    text = after
                    stripped = after
                    break
                else:
                    return "", "noise"
    # Terminal command pattern — compact bash-input + bash-stdout to single label line.
    bin_m = BASH_INPUT_RE.search(text)
    bout_m = BASH_STDOUT_RE.search(text)
    if bin_m or bout_m:
        cmd = bin_m.group(1).strip() if bin_m else ""
        out = bout_m.group(1).strip() if bout_m else ""
        out_short = truncate(out, TRUNC_BASH_STDOUT) if out else ""
        if cmd and out_short:
            return f"$ {cmd}\n{out_short}", "bash"
        if cmd:
            return f"$ {cmd}", "bash"
        if out_short:
            return f"(stdout) {out_short}", "bash"
        return "", "bash-empty"
    return text, "text"


def extract_user_text(content):
    """User content can be a string or a list of items (text/tool_result)."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for item in content:
            if not isinstance(item, dict):
                continue
            if item.get("type") == "text":
                parts.append(item.get("text", ""))
            # tool_result intentionally skipped
        return "\n".join(parts)
    return ""


def parse_session(path):
    sess = {
        "session_id": path.stem,
        "file": str(path),
        "turns": [],
        "tool_histogram": Counter(),
        "bash_categories": Counter(),
        "bash_commands": [],
        "lit_subcommands": Counter(),
        "file_edits": [],
        "file_reads": [],
        "branches": [],  # ordered set
        "cwd": None,
        "model": None,
        "slug": None,
        "custom_title": None,
        "last_prompt": None,
        "first_user_ts": None,
        "last_ts": None,
        "user_turns": 0,
        "assistant_turns": 0,
        "skill_invocations": [],
        "subagent_invocations": [],
        "permission_mode_history": [],
    }
    seen_branches = set()

    with open(path, errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                d = json.loads(line)
            except json.JSONDecodeError:
                continue

            t = d.get("type")
            ts = d.get("timestamp")
            if ts:
                sess["last_ts"] = ts

            br = d.get("gitBranch")
            if br and br not in seen_branches:
                seen_branches.add(br)
                sess["branches"].append(br)
            if d.get("cwd") and not sess["cwd"]:
                sess["cwd"] = d.get("cwd")

            if t == "custom-title":
                sess["custom_title"] = d.get("title") or d.get("customTitle")
                continue
            if t == "last-prompt":
                sess["last_prompt"] = d.get("lastPrompt")
                continue
            if t == "permission-mode":
                sess["permission_mode_history"].append(
                    {"ts": ts, "mode": d.get("mode") or d.get("permissionMode")}
                )
                continue

            if t == "user":
                if not sess["first_user_ts"]:
                    sess["first_user_ts"] = ts
                if d.get("slug") and not sess["slug"]:
                    sess["slug"] = d.get("slug")
                msg = d.get("message", {}) or {}
                raw_text = extract_user_text(msg.get("content", ""))
                if not raw_text or raw_text.strip().startswith("<system-reminder>"):
                    continue
                text, kind = normalize_user_text(raw_text)
                if not text.strip():
                    continue
                sess["user_turns"] += 1
                sess["turns"].append(
                    {
                        "i": len(sess["turns"]),
                        "ts": ts,
                        "role": "user",
                        "kind": kind,
                        "text": truncate(text, TRUNC_USER_TEXT),
                        "branch": br,
                    }
                )
                continue

            if t == "assistant":
                msg = d.get("message", {}) or {}
                if msg.get("model") and not sess["model"]:
                    sess["model"] = msg.get("model")
                content = msg.get("content", []) or []
                turn = {
                    "i": len(sess["turns"]),
                    "ts": ts,
                    "role": "assistant",
                    "thinking": None,
                    "text": None,
                    "tools": [],
                    "branch": br,
                }
                for item in content:
                    if not isinstance(item, dict):
                        continue
                    typ = item.get("type")
                    if typ == "thinking":
                        turn["thinking"] = truncate(
                            item.get("thinking", ""), TRUNC_THINKING
                        )
                    elif typ == "text":
                        turn["text"] = truncate(
                            item.get("text", ""), TRUNC_ASSISTANT_TEXT
                        )
                    elif typ == "tool_use":
                        name = item.get("name", "?")
                        ip = item.get("input") or {}
                        summary = summarize_tool_args(name, ip)
                        turn["tools"].append({"name": name, "args": summary})
                        sess["tool_histogram"][name] += 1
                        if name == "Bash":
                            cmd = ip.get("command", "")
                            sess["bash_commands"].append(truncate(cmd, TRUNC_BASH_CMD))
                            sess["bash_categories"][categorize_bash(cmd)] += 1
                            for sub in extract_lit_subcommands(cmd):
                                sess["lit_subcommands"][sub] += 1
                        elif name in ("Edit", "Write", "NotebookEdit"):
                            sess["file_edits"].append(ip.get("file_path", "?"))
                        elif name == "Read":
                            sess["file_reads"].append(ip.get("file_path", "?"))
                        elif name == "Skill":
                            sess["skill_invocations"].append(
                                {"ts": ts, "skill": ip.get("skill", "?")}
                            )
                        elif name == "Agent":
                            sess["subagent_invocations"].append(
                                {
                                    "ts": ts,
                                    "subagent_type": ip.get(
                                        "subagent_type", "general-purpose"
                                    ),
                                    "description": ip.get("description", ""),
                                }
                            )
                if turn["thinking"] or turn["text"] or turn["tools"]:
                    sess["assistant_turns"] += 1
                    sess["turns"].append(turn)
                continue

            # Other types (attachment, system, file-history-snapshot, etc.) are ignored.

    sess["tool_histogram"] = dict(sess["tool_histogram"])
    sess["bash_categories"] = dict(sess["bash_categories"])
    sess["lit_subcommands"] = dict(sess["lit_subcommands"])
    return sess


def write_session_json(sess, outdir):
    p = outdir / f"{sess['session_id']}.json"
    with open(p, "w") as f:
        json.dump(sess, f, indent=2, default=str)
    return p


def write_session_markdown(sess, outdir):
    p = outdir / f"{sess['session_id']}.md"
    with open(p, "w") as f:
        f.write(f"# Session {sess['session_id']}\n\n")
        f.write(f"- start: `{sess['first_user_ts']}`\n")
        f.write(f"- end: `{sess['last_ts']}`\n")
        f.write(f"- model: `{sess['model']}`\n")
        f.write(f"- branches: {sess['branches']}\n")
        f.write(f"- turns: {len(sess['turns'])} (user={sess['user_turns']}, assistant={sess['assistant_turns']})\n")
        f.write(f"- title: {sess['custom_title'] or '(none)'}\n")
        f.write(f"- last prompt: {truncate(sess['last_prompt'] or '(none)', 200)}\n")
        f.write(f"- cwd: `{sess['cwd']}`\n\n")

        f.write("## Tool histogram\n\n")
        for k, v in sorted(sess["tool_histogram"].items(), key=lambda x: -x[1]):
            f.write(f"- {k}: {v}\n")

        f.write("\n## Bash categories\n\n")
        for k, v in sorted(sess["bash_categories"].items(), key=lambda x: -x[1]):
            f.write(f"- {k}: {v}\n")

        if sess["lit_subcommands"]:
            f.write("\n## lit subcommands\n\n")
            for k, v in sorted(sess["lit_subcommands"].items(), key=lambda x: -x[1]):
                f.write(f"- `lit {k}`: {v}\n")

        if sess["skill_invocations"]:
            f.write("\n## Skills\n\n")
            for s in sess["skill_invocations"]:
                f.write(f"- {s['ts']}: `/{s['skill']}`\n")

        if sess["subagent_invocations"]:
            f.write("\n## Subagents\n\n")
            for s in sess["subagent_invocations"]:
                f.write(f"- {s['ts']}: `{s['subagent_type']}` — {s['description']}\n")

        f.write("\n## Turns\n\n")
        for turn in sess["turns"]:
            f.write(f"### Turn {turn['i']} · {turn['ts']} · {turn['role']}")
            if turn.get("branch"):
                f.write(f" · branch=`{turn['branch']}`")
            f.write("\n\n")
            if turn["role"] == "user":
                f.write(f"```\n{turn['text']}\n```\n\n")
            else:
                if turn.get("thinking"):
                    f.write(f"_thinking_:\n```\n{turn['thinking']}\n```\n\n")
                if turn.get("text"):
                    f.write(f"_text_:\n\n{turn['text']}\n\n")
                if turn.get("tools"):
                    f.write("_tools_:\n\n")
                    for tl in turn["tools"]:
                        f.write(f"- `{tl['name']}` — `{tl['args']}`\n")
                    f.write("\n")
    return p


def write_index(sessions, outdir):
    index = []
    for s in sessions:
        index.append(
            {
                "session_id": s["session_id"],
                "first_user_ts": s["first_user_ts"],
                "last_ts": s["last_ts"],
                "turns": len(s["turns"]),
                "user_turns": s["user_turns"],
                "assistant_turns": s["assistant_turns"],
                "branches": s["branches"],
                "model": s["model"],
                "custom_title": s["custom_title"],
                "last_prompt": truncate(s["last_prompt"] or "", 200),
                "tool_histogram": s["tool_histogram"],
                "bash_categories": s["bash_categories"],
                "lit_subcommands": s["lit_subcommands"],
                "skill_count": len(s["skill_invocations"]),
                "subagent_count": len(s["subagent_invocations"]),
                "edits_count": len(s["file_edits"]),
                "reads_count": len(s["file_reads"]),
            }
        )
    index.sort(key=lambda x: x["first_user_ts"] or "")
    with open(outdir / "index.json", "w") as f:
        json.dump(index, f, indent=2)


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument(
        "--project",
        default=DEFAULT_PROJECT,
        help=f"Project dir name under ~/.claude/projects (default: {DEFAULT_PROJECT})",
    )
    ap.add_argument(
        "--output",
        default=DEFAULT_OUTDIR,
        help=f"Output directory (default: {DEFAULT_OUTDIR})",
    )
    ap.add_argument(
        "--limit", type=int, default=0, help="Process at most N sessions (0 = all)"
    )
    ap.add_argument(
        "--no-markdown",
        action="store_true",
        help="Skip per-session markdown digest (JSON only)",
    )
    args = ap.parse_args()

    proj_dir = Path(os.path.expanduser("~/.claude/projects")) / args.project
    if not proj_dir.exists():
        print(f"error: project dir not found: {proj_dir}", file=sys.stderr)
        return 2

    files = sorted(proj_dir.glob("*.jsonl"))
    if args.limit > 0:
        files = files[: args.limit]
    if not files:
        print(f"error: no .jsonl sessions in {proj_dir}", file=sys.stderr)
        return 2

    outdir = Path(args.output)
    outdir.mkdir(parents=True, exist_ok=True)

    print(f"processing {len(files)} sessions from {proj_dir}")
    sessions = []
    for fp in files:
        try:
            s = parse_session(fp)
        except Exception as e:
            print(f"  {fp.name}: FAILED — {e}", file=sys.stderr)
            continue
        sessions.append(s)
        write_session_json(s, outdir)
        if not args.no_markdown:
            write_session_markdown(s, outdir)
        print(
            f"  {fp.name}: turns={len(s['turns']):4d}  tools={sum(s['tool_histogram'].values()):4d}  bash={sum(s['bash_categories'].values()):4d}  lit={sum(s['lit_subcommands'].values()):3d}"
        )

    write_index(sessions, outdir)
    print(f"\nwrote {len(sessions)} session digests to {outdir}/")
    print(f"index: {outdir / 'index.json'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
