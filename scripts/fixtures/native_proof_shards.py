"""Validate the shard identities declared by the native proof policy."""


def declared_shards(value):
    if not isinstance(value, list) or not value:
        raise ValueError("declared shards must be a nonempty JSON array")
    if any(type(shard) is not int or shard < 0 for shard in value):
        raise ValueError("declared shard identities must be nonnegative integers")
    if len(set(value)) != len(value):
        raise ValueError("declared shard identities must be unique")
    if sorted(value) != list(range(len(value))):
        raise ValueError("declared shards must cover every runner partition")
    return value
