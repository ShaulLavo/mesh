"""Redact an mcp_tee trace for publication.

Usage: redact.py SRC DST PRIVATE_PREFIX...

Only protocol lines (c2s, s2c) are published; server stderr stays private.
Every JSON field named "account" is masked whatever its value. Every string has
the given private prefixes, home directories and the local user name replaced.
The tools/list catalog is reduced to tool names and the mesh_skill text is left
out, because both are large and already in the repository.
"""
import getpass
import json
import os
import re
import sys

HOME = re.compile(r"/(?:home|Users)/[^/\"\s]+")


def scrubber(prefixes):
    replacements = sorted({p for p in prefixes if p}, key=len, reverse=True)
    user = re.compile(r"\b" + re.escape(getpass.getuser()) + r"\b")

    def scrub(text):
        for prefix in replacements:
            text = text.replace(prefix, "$PRIVATE")
        return user.sub("<account>", HOME.sub("<home>", text))
    return scrub


def mask(value, scrub):
    if isinstance(value, dict):
        return {key: "<account>" if key == "account" else mask(item, scrub) for key, item in value.items()}
    if isinstance(value, list):
        return [mask(item, scrub) for item in value]
    if isinstance(value, str):
        return scrub(value)
    return value


class Elider:
    """Remembers which request IDs asked for the bulky payloads, per server process."""

    def __init__(self):
        self.lists, self.skills = set(), set()

    def note(self, pid, message):
        key = (pid, json.dumps(message.get("id")))
        if message.get("method") == "tools/list":
            self.lists.add(key)
        if message.get("method") == "tools/call" and message["params"].get("name") == "mesh_skill":
            self.skills.add(key)

    def shorten(self, pid, message):
        result = message.get("result")
        if not isinstance(result, dict):
            return message
        key = (pid, json.dumps(message.get("id")))
        if key in self.lists and isinstance(result.get("tools"), list):
            names = [tool["name"] for tool in result["tools"]]
            result["tools"] = {"elided": len(names), "names": names}
        if key in self.skills:
            result["content"] = [{"type": "text", "text": "<mesh_skill text elided>"}]
            result["structuredContent"] = {"elided": "mesh_skill envelope"}
        return message


def redact(src, dst, prefixes):
    scrub = scrubber(list(prefixes) + [os.path.expanduser("~")])
    elider = Elider()
    with open(src, encoding="utf-8") as fin, open(dst, "w", encoding="utf-8") as fout:
        for raw in fin:
            record = json.loads(raw)
            if record["dir"] not in ("c2s", "s2c"):
                continue
            try:
                message = json.loads(record["line"])
            except json.JSONDecodeError:
                continue
            if record["dir"] == "c2s":
                elider.note(record["pid"], message)
            message = mask(elider.shorten(record["pid"], message), scrub)
            out = {"t": record["t"], "dir": record["dir"], "pid": record["pid"], "message": message}
            fout.write(json.dumps(out, separators=(",", ":")) + "\n")


if __name__ == "__main__":
    redact(sys.argv[1], sys.argv[2], sys.argv[3:])
