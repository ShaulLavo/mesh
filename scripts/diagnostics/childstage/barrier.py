"""Hold an owned normal-user launcher until kernel hooks report armed."""
import os
import sys


descriptor = int(sys.argv[1])
ready = int(sys.argv[2])
os.write(ready, b"r")
os.close(ready)
if os.read(descriptor, 1) != b"x":
    raise SystemExit("observer did not arm")
os.close(descriptor)
os.execvp(sys.argv[3], sys.argv[3:])
