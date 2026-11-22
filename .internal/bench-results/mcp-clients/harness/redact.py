"""Redact an mcp_tee trace: replace local paths and the account name, and
shorten the two bulky payloads (the tools/list catalog and the mesh_skill text)."""
import json
import sys

src, dst, root, scratch, account = sys.argv[1:6]
REPL = [(root, "$ROOT"), (scratch, "$SCRATCH")]
list_ids = set()
skill_ids = set()


def scrub(text):
    for old, new in REPL:
        text = text.replace(old, new)
    return text.replace(f'"account":"{account}"', '"account":"<account>"')


def shorten(message, direction):
    if direction == "c2s" and message.get("method") == "tools/list":
        list_ids.add(json.dumps(message.get("id")))
    if direction == "c2s" and message.get("method") == "tools/call" and message["params"].get("name") == "mesh_skill":
        skill_ids.add(json.dumps(message.get("id")))
    if direction != "s2c" or "result" not in message:
        return message
    key = json.dumps(message.get("id"))
    result = message["result"]
    if key in list_ids and "tools" in result:
        names = [tool["name"] for tool in result["tools"]]
        result["tools"] = f"<{len(names)} tool definitions elided: {', '.join(names)}>"
    if key in skill_ids:
        result["content"] = "<mesh_skill text elided>"
        result["structuredContent"] = "<mesh_skill envelope elided>"
    return message


with open(src, encoding="utf-8") as fin, open(dst, "w", encoding="utf-8") as fout:
    for raw in fin:
        record = json.loads(raw)
        line = record["line"]
        try:
            message = shorten(json.loads(line), record["dir"])
            line = json.dumps(message, separators=(",", ":"))
        except (json.JSONDecodeError, KeyError, TypeError):
            pass
        record["line"] = scrub(line)
        fout.write(json.dumps(record) + "\n")
