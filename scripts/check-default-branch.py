import re
import subprocess
import sys
import time


TRANSIENT_HTTP = {500, 502, 503, 504}
HTTP_STATUS = re.compile(r"\(HTTP (\d{3})\)")
ATTEMPTS = 3
TOTAL_SECONDS = 60
ATTEMPT_SECONDS = 20


def check(repository):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("Specify the GitHub repository as owner/name.")
    if any(part in (".", "..") for part in repository.split("/")):
        raise ValueError("Specify the GitHub repository as owner/name.")
    deadline = time.monotonic() + TOTAL_SECONDS
    for attempt in range(ATTEMPTS):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Default-branch verification reached its time limit.")
        result = subprocess.run(
            ["gh", "api", f"repos/{repository}", "--jq", ".default_branch"],
            capture_output=True, text=True, timeout=min(ATTEMPT_SECONDS, remaining),
        )
        if result.returncode == 0:
            if result.stdout.strip() != "main":
                raise ValueError("The repository default branch must be main.")
            return
        match = HTTP_STATUS.search(result.stderr)
        status = int(match.group(1)) if match else None
        if status not in TRANSIENT_HTTP or attempt == ATTEMPTS - 1:
            raise RuntimeError(f"GitHub default-branch read failed (HTTP status {status}, exit {result.returncode}).")
        delay = 0.5 * (2 ** attempt)
        if deadline - time.monotonic() <= delay:
            raise TimeoutError("Default-branch verification reached its time limit.")
        time.sleep(delay)


def main():
    if len(sys.argv) != 2:
        print("Usage: check-default-branch.py owner/name", file=sys.stderr)
        return 1
    try:
        check(sys.argv[1])
    except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as error:
        if isinstance(error, subprocess.TimeoutExpired):
            print("GitHub default-branch read timed out.", file=sys.stderr)
        else:
            print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
