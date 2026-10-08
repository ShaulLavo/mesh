import stat


def snapshot(*directories):
    result = {}
    for directory in directories:
        paths = [directory, *directory.rglob("*")]
        for path in paths:
            try:
                mode = path.stat().st_mode
            except FileNotFoundError:
                if path != directory:
                    raise AssertionError(f"fixture entry disappeared during snapshot: {path}") from None
                result[str(path)] = None
                continue
            contents = path.read_bytes() if stat.S_ISREG(mode) else None
            result[str(path)] = (mode, contents)
    return result


def describe(snapshot_value, path):
    if path not in snapshot_value:
        return "unlisted"
    entry = snapshot_value[path]
    if entry is None:
        return "absent"
    mode, contents = entry
    detail = f"mode={mode:o}"
    if contents is not None:
        detail += f" bytes={len(contents)}"
    return detail


def assert_unchanged(before, after, operation):
    missing = object()
    changed = sorted(path for path in before.keys() | after.keys()
                     if before.get(path, missing) != after.get(path, missing))
    details = []
    for path in changed:
        previous, current = before.get(path), after.get(path)
        contents_changed = previous is not None and current is not None and previous[1] != current[1]
        details.append(f"{path}: {describe(before, path)} -> {describe(after, path)}; contents changed={contents_changed}")
    assert after == before, f"{operation} changed fixture files/directories: {details}"
