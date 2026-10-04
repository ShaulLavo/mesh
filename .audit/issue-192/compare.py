from collections import defaultdict
from pathlib import Path
import re
from statistics import median


def samples(filename):
    values = defaultdict(list)
    for line in Path(__file__).with_name(filename).read_text().splitlines():
        match = re.fullmatch(
            r"BenchmarkLoadHostsClaimCache/hosts-(\d+)/writers-(true|false)-\d+\s+\d+\s+(\d+) ns/op",
            line.strip(),
        )
        if match is None:
            continue
        values[int(match[1]), match[2]].append(int(match[3]))
    return values


before, after = samples("before.txt"), samples("after.txt")
assert len(before) == 6 and before.keys() == after.keys()
print("| Hosts | Writers | Before ms/op | After ms/op | Reduction |")
print("| --- | --- | --- | --- | --- |")
for key in sorted(before):
    assert len(before[key]) == len(after[key]) == 5
    old, new = median(before[key]), median(after[key])
    print(f"| {key[0]} | {key[1]} | {old / 1e6:.6f} | {new / 1e6:.6f} | {100 * (1 - new / old):.1f}% |")
